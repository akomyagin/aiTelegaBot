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
// returns the trimmed summary content.
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
