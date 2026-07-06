// Package digest orchestrates one digest run: collect items from all sources →
// filter unseen → summarize via LLM → render → deliver → mark seen / save
// history. See the data-flow in docs/TECHNICAL_PLAN.md §3.1.
//
// Stage 0: wiring + stub Run — real pipeline lands in Этап 4.
package digest

import (
	"context"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
	"github.com/akomyagin/aiTelegaBot/internal/llm"
	"github.com/akomyagin/aiTelegaBot/internal/storage"
	"github.com/akomyagin/aiTelegaBot/internal/telegram"
)

// Pipeline holds the dependencies for one digest run.
type Pipeline struct {
	Sources   []feed.Source
	Store     storage.Store
	Summarize llm.Summarizer
	Deliver   telegram.Deliverer
	ChatID    string
}

// Run executes one digest end-to-end. Stage 0: stub.
func (p *Pipeline) Run(ctx context.Context) error {
	_ = ctx
	return nil
}

// Render turns a summary into the Telegram-ready message body. Stage 0: stub.
func Render(summary string, items []feed.Item) string {
	_ = items
	return summary
}
