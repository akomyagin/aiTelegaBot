// Offline summarizer: deterministic, extractive, no network. Activated when no
// LLM API key is configured (BYOK) or forced for dev/CI. Same interface as the
// networked client so it substitutes transparently. See SKILL.md §4.
//
// Determinism is required: same input always yields the same output — no
// random, no map iteration, no time.Now().
package llm

import (
	"context"
	"strings"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
)

// leadRunes caps the extracted lead sentence length (in runes).
const leadRunes = 200

// Offline is a deterministic extractive summarizer (no network).
type Offline struct{}

// NewOffline builds the offline summarizer.
func NewOffline() *Offline { return &Offline{} }

// Summarize returns a bullet list, one line per item, in input order.
func (o *Offline) Summarize(ctx context.Context, items []feed.Item) (string, error) {
	_ = ctx
	if len(items) == 0 {
		return "Нет новых материалов.", nil
	}
	lines := make([]string, 0, len(items))
	for _, it := range items {
		lead := extractLead(it.Text)
		var b strings.Builder
		b.WriteString("- ")
		b.WriteString(it.Title)
		if lead != "" {
			b.WriteString(": ")
			b.WriteString(lead)
		}
		if it.URL != "" {
			b.WriteString(" (")
			b.WriteString(it.URL)
			b.WriteString(")")
		}
		lines = append(lines, b.String())
	}
	return strings.Join(lines, "\n"), nil
}

// extractLead takes the first ~leadRunes runes of text, or everything up to the
// first ". " sentence boundary, whichever is shorter. Deterministic.
func extractLead(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if idx := strings.Index(text, ". "); idx >= 0 {
		text = text[:idx+1] // keep the period
	}
	return truncateRunes(text, leadRunes)
}
