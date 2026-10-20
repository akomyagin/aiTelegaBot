// Package llm provides the LLM client used to summarize collected items into a
// digest.
//
// The networked client is a hand-written net/http client (no SDK) with
// retry+backoff+jitter (see docs/TECHNICAL_PLAN.md §2 and SKILL.md §4). When no
// API key is configured (BYOK), the deterministic offline summarizer is used
// instead. The API key is never logged, even on error.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
)

const (
	defaultBaseURL    = "https://api.openai.com/v1"
	defaultModel      = "gpt-4o-mini"
	defaultMaxRetries = 3
	requestTimeout    = 60 * time.Second
)

// Summarizer turns a batch of items into a digest summary. Both the networked
// client and the offline summarizer implement it.
type Summarizer interface {
	Summarize(ctx context.Context, items []feed.Item) (string, error)
}

// Client is the hand-written net/http client to an OpenAI-compatible API.
// The API key is never logged.
type Client struct {
	apiKey     string // never logged
	baseURL    string
	model      string
	http       *http.Client
	maxRetries int
	log        *slog.Logger
}

// NewClient builds a networked summarizer with retry+backoff. Empty baseURL,
// model or non-positive maxRetries fall back to sensible defaults.
func NewClient(apiKey, baseURL, model string, maxRetries int) *Client {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	if model == "" {
		model = defaultModel
	}
	if maxRetries <= 0 {
		maxRetries = defaultMaxRetries
	}
	return &Client{
		apiKey:     apiKey,
		baseURL:    strings.TrimRight(baseURL, "/"),
		model:      model,
		http:       &http.Client{Timeout: requestTimeout},
		maxRetries: maxRetries,
		log:        slog.Default(),
	}
}

type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []message `json:"messages"`
	Temperature float64   `json:"temperature"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Summarize calls the LLM chat-completions endpoint with retry+backoff and
// returns the trimmed summary content. Техдолг B L4: the response goes
// through extractContent — one truth about decode/api-error/empty-choices
// parsing shared with SuggestTopic and SummarizeTopic (behavior unchanged).
func (c *Client) Summarize(ctx context.Context, items []feed.Item) (string, error) {
	msgs := buildPrompt(items)
	body, err := json.Marshal(chatRequest{
		Model:       c.model,
		Messages:    msgs,
		Temperature: 0.2,
	})
	if err != nil {
		return "", fmt.Errorf("llm: marshal request: %w", err)
	}

	url := c.baseURL + "/chat/completions"
	makeReq := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+c.apiKey) // apiKey never logged
		return req, nil
	}

	resp, err := c.doWithRetry(ctx, makeReq)
	if err != nil {
		return "", err
	}
	return extractContent(resp)
}

// SuggestTopic implements TopicSuggester over the chat-completions API. The
// model is asked for strict JSON {"topic":"<имя>"}; HTTP-level failures are
// retried by doWithRetry as usual, and an unparseable answer triggers exactly
// ONE additional request before giving up with an error (plan §4.10). An answer
// that is not in existing is fine — it is accepted as a new topic name; the
// caller decides what to do with it. Empty name = "предложить нечего".
// The API key is never logged.
func (c *Client) SuggestTopic(ctx context.Context, existing []string, sample []feed.Item) (string, error) {
	msgs := buildTopicPrompt(existing, sample)
	body, err := json.Marshal(chatRequest{
		Model:       c.model,
		Messages:    msgs,
		Temperature: 0.2,
	})
	if err != nil {
		return "", fmt.Errorf("llm: marshal request: %w", err)
	}

	url := c.baseURL + "/chat/completions"
	makeReq := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+c.apiKey) // apiKey never logged
		return req, nil
	}

	const maxParseAttempts = 2 // initial request + one repeat on an unparseable answer
	var lastErr error
	for attempt := 0; attempt < maxParseAttempts; attempt++ {
		resp, err := c.doWithRetry(ctx, makeReq)
		if err != nil {
			// HTTP-level failures were already retried per maxRetries inside
			// doWithRetry; fatal errors (401/400) must not be re-requested.
			return "", err
		}
		content, err := extractContent(resp)
		if err != nil {
			return "", err
		}
		name, perr := parseTopicSuggestion(content)
		if perr == nil {
			return name, nil
		}
		lastErr = perr
		c.log.Warn("llm topic suggestion unparseable", "attempt", attempt, "err", perr)
	}
	return "", fmt.Errorf("llm: topic suggestion unparseable after %d attempts: %w", maxParseAttempts, lastErr)
}

// SummarizeTopic implements TopicSummarizer over the chat-completions API —
// the SuggestTopic pattern verbatim (plan §4.10 / Stage 8 C): strict JSON
// {"headline":"…","items":[{"index":1,"summary":"…"}]}; transport/HTTP errors
// surface immediately (doWithRetry already retried; fatal 400/401 are not
// re-requested); an unparseable answer triggers exactly ONE repeat; if BOTH
// answers are unparseable — fallback: Headline = the plain Summarize result,
// Items empty (L2 then renders from item titles — degradation, not failure).
// The API key is never logged.
func (c *Client) SummarizeTopic(ctx context.Context, topicName string, items []feed.Item) (TopicSummary, error) {
	msgs := buildTopicDigestPrompt(topicName, items)
	body, err := json.Marshal(chatRequest{
		Model:       c.model,
		Messages:    msgs,
		Temperature: 0.2,
	})
	if err != nil {
		return TopicSummary{}, fmt.Errorf("llm: marshal request: %w", err)
	}

	url := c.baseURL + "/chat/completions"
	makeReq := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+c.apiKey) // apiKey never logged
		return req, nil
	}

	const maxParseAttempts = 2 // initial request + one repeat on an unparseable answer
	var lastErr error
	for attempt := 0; attempt < maxParseAttempts; attempt++ {
		resp, err := c.doWithRetry(ctx, makeReq)
		if err != nil {
			return TopicSummary{}, err
		}
		content, err := extractContent(resp)
		if err != nil {
			return TopicSummary{}, err
		}
		ts, perr := parseTopicSummary(content, len(items))
		if perr == nil {
			return ts, nil
		}
		lastErr = perr
		c.log.Warn("llm topic summary unparseable", "attempt", attempt, "err", perr)
	}
	// Fallback (§4.10): plain Summarize as the headline, Items stay empty.
	s, serr := c.Summarize(ctx, items)
	if serr != nil {
		return TopicSummary{}, fmt.Errorf("llm: topic summary unparseable after %d attempts (%v); fallback summarize failed: %w",
			maxParseAttempts, lastErr, serr)
	}
	return TopicSummary{Headline: s}, nil
}

// extractContent reads the first choice's content from an open chat-completions
// response and closes the body.
func extractContent(resp *http.Response) (string, error) {
	defer func() { _ = resp.Body.Close() }()
	var parsed chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", fmt.Errorf("llm: decode response: %w", err)
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("llm: api error: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("llm: empty choices in response")
	}
	return strings.TrimSpace(parsed.Choices[0].Message.Content), nil
}
