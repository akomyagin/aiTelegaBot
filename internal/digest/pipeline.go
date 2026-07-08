// Package digest orchestrates one digest run: collect items from all sources →
// filter unseen → summarize via LLM → render → deliver → mark seen / save
// history. See the data-flow in docs/TECHNICAL_PLAN.md §3.1.
//
// Delivery gates persistence: items are marked seen and the digest is saved
// only after a successful Deliver, so a failed delivery is retried next run.
//
// Этап 4: full pipeline + Render.
package digest

import (
	"context"
	"fmt"
	"log/slog"
	"time"

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
	Log       *slog.Logger     // optional; defaults to slog.Default()
	Now       func() time.Time // optional; defaults to time.Now (for tests)
}

// log returns the configured logger or the default.
func (p *Pipeline) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

// Run executes one digest end-to-end.
func (p *Pipeline) Run(ctx context.Context) error {
	log := p.log()

	// 1. Collect from all sources (partial failures are non-fatal).
	items, collectErr := feed.Collect(ctx, p.Sources)
	if collectErr != nil {
		log.Warn("partial source errors", "err", collectErr)
	}

	// 2. Drop items already delivered in a previous run.
	fresh, err := p.Store.FilterUnseen(ctx, items)
	if err != nil {
		return fmt.Errorf("filter unseen: %w", err)
	}

	// 3. Nothing new — exit quietly (no empty digest).
	if len(fresh) == 0 {
		log.Info("no new items, skipping digest")
		return nil
	}

	// 4. Summarize.
	summary, err := p.Summarize.Summarize(ctx, fresh)
	if err != nil {
		return fmt.Errorf("summarize: %w", err)
	}

	// 5. Render the Telegram body.
	now := time.Now()
	if p.Now != nil {
		now = p.Now()
	}
	body := Render(summary, fresh, now)

	// 6. Deliver. On failure we do NOT mark seen, so the run is repeatable.
	if err := p.Deliver.Deliver(ctx, p.ChatID, body); err != nil {
		return fmt.Errorf("deliver: %w", err)
	}

	// 7. Persist only after successful delivery. These errors are logged but do
	// not fail the run — the digest already reached the user.
	if err := p.Store.MarkSeen(ctx, fresh); err != nil {
		log.Error("mark seen failed", "err", err)
	}
	if err := p.Store.SaveDigest(ctx, body, len(fresh)); err != nil {
		log.Error("save digest failed", "err", err)
	}
	log.Info("digest delivered", "items", len(fresh))
	return nil
}

// Render turns a summary into the Telegram-ready message body. now supplies the
// header date deterministically. Special characters in summary are the LLM's
// responsibility (ParseMode HTML).
func Render(summary string, items []feed.Item, now time.Time) string {
	return fmt.Sprintf(
		"📋 Дайджест — %s\n\n%s\n\n— %d материалов",
		now.Format("02 Jan 2006"),
		summary,
		len(items),
	)
}
