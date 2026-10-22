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

// SummarizeTopic (TopicSummarizer, Этап 8) УДАЛЁН в 9D-2b: вызов C
// (AggregateTopic, aggregate.go) заменил его в pipeline.Run — production-
// вызовов не осталось (пункт E мандата 9D-2b, решение зафиксировано в отчёте).

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

// ClassifySummarize implements ClassifySummarizer (вызов A плана §4.8) over
// the chat-completions API. Элементы чанкуются по classifyChunkSize на один
// HTTP-запрос (план, строки 555–557); индексы в результате — глобальные
// (1..len(items)). На чанк — паттерн SummarizeTopic: fence-strip, строгий
// JSON, ровно ОДИН повтор при мусорном ответе (максимум 2 запроса на чанк),
// существующий retry/backoff транспорта (doWithRetry).
//
// Ошибка транспорта/API → error немедленно (тема в pipeline пропускается
// целиком, план §4.9); при этом возвращается ЧАСТИЧНЫЙ результат — успешно
// обработанные чанки с глобальными индексами (вызывающий кэширует не-Degraded
// часть до обработки сбоя, чтобы следующий прогон не переспрашивал LLM).
// Двойной мусор на чанк → НЕ ошибка: fallback (план,
// строки 480–484) — все элементы чанка получают class=other,
// summary=extractLead, Degraded=true (вызывающий не пишет им status=done).
// API-ключ никогда не логируется.
func (c *Client) ClassifySummarize(ctx context.Context, items []ClassifyItem) (ClassifyResult, error) {
	if len(items) == 0 {
		return ClassifyResult{}, nil
	}
	var out ClassifyResult
	for start := 0; start < len(items); start += classifyChunkSize {
		end := min(start+classifyChunkSize, len(items))
		res, err := c.classifyChunk(ctx, items[start:end])
		if err != nil {
			// Частичный результат: уже обработанные чанки не теряются (M1).
			return out, err
		}
		for i := range res.Items {
			res.Items[i].Index += start // локальный индекс чанка → глобальный
		}
		out.Items = append(out.Items, res.Items...)
		out.Degraded = append(out.Degraded, res.Degraded...)
	}
	return out, nil
}

// classifyChunk — один HTTP-запрос вызова A на чанк ≤classifyChunkSize
// элементов: SummarizeTopic-паттерн (2 попытки парсинга максимум, затем
// детерминированный fallback без ошибки).
func (c *Client) classifyChunk(ctx context.Context, chunk []ClassifyItem) (ClassifyResult, error) {
	msgs := buildClassifyPrompt(chunk)
	body, err := json.Marshal(chatRequest{
		Model:       c.model,
		Messages:    msgs,
		Temperature: 0.2,
	})
	if err != nil {
		return ClassifyResult{}, fmt.Errorf("llm: marshal request: %w", err)
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
			// HTTP-level failures were already retried inside doWithRetry;
			// fatal errors (400/401) must not be re-requested.
			return ClassifyResult{}, err
		}
		content, err := extractContent(resp)
		if err != nil {
			return ClassifyResult{}, err
		}
		res, perr := parseClassifyResponse(content, chunk)
		if perr == nil {
			return res, nil
		}
		lastErr = perr
		c.log.Warn("llm classify response unparseable", "attempt", attempt, "err", perr)
	}
	// Fallback (§4.8): оба ответа мусорные — тема НЕ пропускается, весь чанк
	// деградирует детерминированно; Degraded=true не даёт вызывающему написать
	// status=done (следующий прогон переспросит LLM).
	c.log.Warn("llm classify falling back to deterministic summaries",
		"chunk_items", len(chunk), "err", lastErr)
	return degradedClassifyResult(chunk), nil
}

// ClassifyLinks implements LinkClassifier (вызов E плана §4.8) over the
// chat-completions API — один запрос на тему (план: гранулярность «1 на тему»,
// чанкования нет). Паттерн SuggestTopic/SummarizeTopic: строгий JSON
// {"links":[{"msg":1,"link":2,"kind":"article"}]}, fence-strip, ровно ОДИН
// повтор при мусорном ответе (максимум 2 запроса), существующий retry/backoff
// транспорта (doWithRetry).
//
// В отличие от вызова A, двойной мусор → ERROR без fallback-результата:
// деградирует вызывающий (pipeline оставит ссылки в item_links с
// kind=other по умолчанию — план §4.3). Ошибка транспорта/API → error
// немедленно. Контракт покрытия LinkClassifier: РОВНО по одному LinkKind на
// пару (msg,link) входа; отсутствующая в ответе пара → kind=other. Пустой
// вход (нет ни одной пары) → пустой результат БЕЗ HTTP-запроса.
// API-ключ никогда не логируется.
func (c *Client) ClassifyLinks(ctx context.Context, msgs []MsgLinks) ([]LinkKind, error) {
	pairs := allLinkPairs(msgs)
	if len(pairs) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(chatRequest{
		Model:       c.model,
		Messages:    buildLinkClassifyPrompt(msgs),
		Temperature: 0.2,
	})
	if err != nil {
		return nil, fmt.Errorf("llm: marshal request: %w", err)
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
			// HTTP-level failures were already retried inside doWithRetry;
			// fatal errors (400/401) must not be re-requested.
			return nil, err
		}
		content, err := extractContent(resp)
		if err != nil {
			return nil, err
		}
		kinds, perr := parseLinksResponse(content, msgs)
		if perr == nil {
			return kinds, nil
		}
		lastErr = perr
		c.log.Warn("llm links classify unparseable", "attempt", attempt, "err", perr)
	}
	return nil, fmt.Errorf("llm: links classify unparseable after %d attempts: %w", maxParseAttempts, lastErr)
}

// SummarizeLinks implements LinkSummarizer (вызов F плана §4.8) over the
// chat-completions API — ClassifyLinks-паттерн: один запрос на тему, строгий
// JSON {"items":[{"index":1,"summary":"…"}]}, fence-strip, ровно ОДИН повтор
// при мусоре, затем ERROR (деградация вызывающего: ссылки останутся без
// саммари, план §4.3). Text каждой страницы обрезается до
// linkSummaryTextRunes (1500) рун в промпте. Контракт покрытия: РОВНО по
// одному LinkSummary на страницу, Index 1..n; отсутствующий индекс →
// Summary="" (не ошибка). Пустой вход → пустой результат без HTTP-запроса.
// API-ключ никогда не логируется.
func (c *Client) SummarizeLinks(ctx context.Context, pages []LinkPage) ([]LinkSummary, error) {
	if len(pages) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(chatRequest{
		Model:       c.model,
		Messages:    buildLinkSummaryPrompt(pages),
		Temperature: 0.2,
	})
	if err != nil {
		return nil, fmt.Errorf("llm: marshal request: %w", err)
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
			return nil, err
		}
		content, err := extractContent(resp)
		if err != nil {
			return nil, err
		}
		sums, perr := parseLinkSummaries(content, len(pages))
		if perr == nil {
			return sums, nil
		}
		lastErr = perr
		c.log.Warn("llm link summaries unparseable", "attempt", attempt, "err", perr)
	}
	return nil, fmt.Errorf("llm: link summaries unparseable after %d attempts: %w", maxParseAttempts, lastErr)
}

// AggregateTopic реализует TopicAggregator (вызов C плана §4.8) поверх
// chat-completions API — паттерн SummarizeTopic: строгий JSON
// {"headline":"…","sections":[{"source_index":1,"summary":"…"}]}, fence-strip,
// ровно ОДИН повтор при мусорном ответе, существующий retry/backoff транспорта
// (doWithRetry). Пустой вход → пустой результат БЕЗ HTTP-запроса
// (ClassifyLinks-паттерн).
//
// Ошибка транспорта/API → error немедленно (9D-2 обработает по плану:
// fallback-headline темы, тема НЕ пропускается — шаг 4d). Двойной мусор → НЕ
// ошибка: детерминированный fallback (план 9D, тест-кейс 1) — Headline =
// первые ≤3 саммари сообщений списком «- …» (порядок среза входа), Sections
// пусты (L1 рендерит строки топиков без саммари — деградация). API-ключ
// никогда не логируется.
func (c *Client) AggregateTopic(ctx context.Context, topicName string, sources []SourceBlock) (TopicAggregate, error) {
	if len(sources) == 0 {
		return TopicAggregate{}, nil
	}
	body, err := json.Marshal(chatRequest{
		Model:       c.model,
		Messages:    buildAggregateTopicPrompt(topicName, sources),
		Temperature: 0.2,
	})
	if err != nil {
		return TopicAggregate{}, fmt.Errorf("llm: marshal request: %w", err)
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
			// HTTP-level failures were already retried inside doWithRetry;
			// fatal errors (400/401) must not be re-requested.
			return TopicAggregate{}, err
		}
		content, err := extractContent(resp)
		if err != nil {
			return TopicAggregate{}, err
		}
		agg, perr := parseAggregateTopic(content, sources)
		if perr == nil {
			return agg, nil
		}
		lastErr = perr
		c.log.Warn("llm topic aggregate unparseable", "attempt", attempt, "err", perr)
	}
	c.log.Warn("llm topic aggregate falling back to first summaries",
		"sources", len(sources), "err", lastErr)
	return degradedTopicAggregate(sources), nil
}

// AggregateOverall реализует OverallAggregator (вызов D плана §4.8) — один
// запрос на прогон: строгий JSON {"summary":"…"}, fence-strip, ровно ОДИН
// повтор при мусоре. РЕШЕНИЕ 9D-1 (план §4.8.1 шаг 6 — «сбой D — log-only,
// L0 без общего саммари (деградация, не ошибка)»): двойной мусор → НЕ ошибка,
// overall "" — TOC рендерится без overall-блока; ошибка транспорта/API →
// error (9D-2 тоже обработает его log-only, но отличает транспортный сбой от
// мусорного ответа в логах). Пустой вход → ("", nil) без HTTP-запроса.
// API-ключ никогда не логируется.
func (c *Client) AggregateOverall(ctx context.Context, topics []OverallTopic) (string, error) {
	if len(topics) == 0 {
		return "", nil
	}
	body, err := json.Marshal(chatRequest{
		Model:       c.model,
		Messages:    buildAggregateOverallPrompt(topics),
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
			return "", err
		}
		content, err := extractContent(resp)
		if err != nil {
			return "", err
		}
		s, perr := parseOverallSummary(content)
		if perr == nil {
			return s, nil
		}
		lastErr = perr
		c.log.Warn("llm overall summary unparseable", "attempt", attempt, "err", perr)
	}
	c.log.Warn("llm overall summary falling back to empty", "topics", len(topics), "err", lastErr)
	return "", nil
}
