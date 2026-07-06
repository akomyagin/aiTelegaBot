// Offline summarizer: deterministic, extractive, no network. Activated when no
// LLM API key is configured (BYOK) or forced for dev/CI. Same interface as the
// networked client so it substitutes transparently. See SKILL.md §4.
//
// Stage 0: stub — real extractive logic lands in Этап 3.
package llm

import (
	"context"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
)

// Offline is a deterministic extractive summarizer (no network).
type Offline struct{}

// NewOffline builds the offline summarizer.
func NewOffline() *Offline { return &Offline{} }

// Summarize produces a deterministic digest from items. Stage 0: stub.
func (o *Offline) Summarize(ctx context.Context, items []feed.Item) (string, error) {
	_ = ctx
	_ = items
	return "", nil
}
