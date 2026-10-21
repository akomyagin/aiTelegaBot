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

// SuggestTopic implements TopicSuggester offline: deterministically no
// suggestion ("", nil) — the offline summarizer never proposes topics
// (plan §4.10).
func (o *Offline) SuggestTopic(ctx context.Context, existing []string, sample []feed.Item) (string, error) {
	_ = ctx
	_ = existing
	_ = sample
	return "", nil
}

// SummarizeTopic implements TopicSummarizer offline, deterministically
// (plan §4.10): Headline — up to 3 first "- <Title>" lines joined by "\n"
// (input order); Items — the first sentence of Text per item (Title when
// Text is empty). No time/random/map iteration.
func (o *Offline) SummarizeTopic(ctx context.Context, topicName string, items []feed.Item) (TopicSummary, error) {
	_ = ctx
	_ = topicName
	const maxHeadlineLines = 3
	lines := make([]string, 0, maxHeadlineLines)
	for i, it := range items {
		if i >= maxHeadlineLines {
			break
		}
		lines = append(lines, "- "+it.Title)
	}
	ts := TopicSummary{Headline: strings.Join(lines, "\n")}
	for i, it := range items {
		summary := extractLead(it.Text)
		if summary == "" {
			summary = it.Title
		}
		ts.Items = append(ts.Items, ItemSummary{Index: i + 1, Summary: summary})
	}
	return ts, nil
}

// ClassifySummarize implements ClassifySummarizer offline (план §4.1):
// детерминированная деградация без сети — все элементы class=other (реклама
// НЕ фильтруется — осознанно: ложное срабатывание эвристики хуже пропуска),
// summary = extractLead(Text), при пустом Text — Title. Degraded=true для
// всех: выход не LLM-качества, pipeline не должен фиксировать его как
// окончательный (status=done) — см. ClassifyResult. Два вызова на одном входе
// байт-в-байт одинаковы: нет сети, random, time.Now и map-итерации.
func (o *Offline) ClassifySummarize(ctx context.Context, items []ClassifyItem) (ClassifyResult, error) {
	_ = ctx
	return degradedClassifyResult(items), nil
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
