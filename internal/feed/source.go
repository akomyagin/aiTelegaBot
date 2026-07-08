// Package feed collects content from web sources (RSS/Atom, arXiv, Hacker News)
// and normalizes it into a single Item type shared across the pipeline.
//
// Telegram sources live in internal/telegram (Bot API) and internal/mtproto
// (private channels, Фаза 2); they produce the same Item.
//
// Этап 2: RSS/Atom/arXiv and Hacker News collectors (rss.go, hn.go).
package feed

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// timeZero is the zero Published time used when a source provides no date.
var timeZero = time.Time{}

// Item is the normalized unit of content flowing through the digest pipeline,
// regardless of origin (web feed or Telegram).
type Item struct {
	Source    string // human-readable source name
	Kind      string // "rss" | "arxiv" | "hn" | "tg_botapi" | "tg_public" | "tg_mtproto"
	Title     string
	URL       string
	Text      string // body / abstract / message text
	Published time.Time
	ID        string // message ID for Telegram sources (tg_botapi, tg_public)
}

// DedupKey returns the stable key used to skip already-seen items across runs
// (see docs/TECHNICAL_PLAN.md §7). Stage 0: placeholder.
func (i Item) DedupKey() string {
	// TG sources are deduplicated by message ID, not URL (the URL can change
	// when a post is edited).
	if strings.HasPrefix(i.Kind, "tg_") && i.ID != "" {
		return "tg:" + i.Source + ":" + i.ID
	}
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

// Collect gathers items from all given sources. A single source failure is
// recorded but does not abort the digest — partial results are returned along
// with a joined error.
func Collect(ctx context.Context, sources []Source) ([]Item, error) {
	var all []Item
	var errs []error
	for _, src := range sources {
		items, err := src.Collect(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", src.Name(), err))
			continue
		}
		all = append(all, items...)
	}
	return all, errors.Join(errs...)
}
