// Package llm provides the LLM client used to summarize collected items into a
// digest.
//
// The networked client is a hand-written net/http client (no SDK) with
// retry+backoff+jitter (see docs/TECHNICAL_PLAN.md §2 and SKILL.md §4). When no
// API key is configured (BYOK), the deterministic offline summarizer is used
// instead.
//
// Stage 0: interface + stubs only — real client lands in Этап 3.
package llm

import (
	"context"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
)

// Summarizer turns a batch of items into a digest summary. Both the networked
// client and the offline summarizer implement it.
type Summarizer interface {
	Summarize(ctx context.Context, items []feed.Item) (string, error)
}

// Client is the hand-written net/http client to an OpenAI-compatible API.
// The API key is never logged. Stage 0: stub.
type Client struct {
	apiKey  string // never logged
	baseURL string
	model   string
}

// NewClient builds a networked summarizer. Stage 0: stub.
func NewClient(apiKey, baseURL, model string) *Client {
	return &Client{apiKey: apiKey, baseURL: baseURL, model: model}
}

// Summarize calls the LLM with retry+backoff. Stage 0: stub.
func (c *Client) Summarize(ctx context.Context, items []feed.Item) (string, error) {
	_ = ctx
	_ = items
	return "", nil
}
