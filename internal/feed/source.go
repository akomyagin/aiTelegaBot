// Package feed collects content from web sources (RSS/Atom, arXiv, Hacker News)
// and normalizes it into a single Item type shared across the pipeline.
//
// Telegram sources live in internal/telegram (Bot API) and internal/mtproto
// (private channels, Фаза 2); they produce the same Item.
//
// Stage 0: types and interface only — collectors land in Этап 2.
package feed

import (
	"context"
	"time"
)

// Item is the normalized unit of content flowing through the digest pipeline,
// regardless of origin (web feed or Telegram).
type Item struct {
	Source    string // human-readable source name
	Kind      string // "rss" | "arxiv" | "hn" | "tg_botapi" | "tg_public" | "tg_mtproto"
	Title     string
	URL       string
	Text      string // body / abstract / message text
	Published time.Time
}

// DedupKey returns the stable key used to skip already-seen items across runs
// (see docs/TECHNICAL_PLAN.md §7). Stage 0: placeholder.
func (i Item) DedupKey() string {
	if i.URL != "" {
		return i.URL
	}
	return i.Kind + ":" + i.Title
}

// Source reads a batch of items from one origin.
type Source interface {
	Collect(ctx context.Context) ([]Item, error)
	Name() string
}

// Collect gathers items from all given sources. Stage 0: stub.
func Collect(ctx context.Context, sources []Source) ([]Item, error) {
	_ = ctx
	_ = sources
	return nil, nil
}
