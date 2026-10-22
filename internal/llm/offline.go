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

// SummarizeTopic (TopicSummarizer, Этап 8) УДАЛЁН в 9D-2b: вызов C
// (AggregateTopic ниже) заменил его в pipeline.Run; паттерн offline-headline
// «первые ≤3 строки» живёт в firstSummaryLines/degradedTopicAggregate
// (aggregate.go) и в digest.fallbackHeadline (пункт E мандата 9D-2b).

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

// ClassifyLinks implements LinkClassifier offline (план §4.7):
// детерминированно все пары (msg,link) получают kind=other — без сети.
// Семантика: «неизвестное считаем статьёй» (§4.3), но pipeline в offline-режиме
// не fetch'ит вовсе (item_links пишутся с kind=other без саммари). Контракт
// покрытия LinkClassifier соблюдён: ровно по одному LinkKind на пару в порядке
// входа. Два вызова на одном входе байт-в-байт одинаковы.
func (o *Offline) ClassifyLinks(ctx context.Context, msgs []MsgLinks) ([]LinkKind, error) {
	_ = ctx
	return allLinkPairs(msgs), nil
}

// SummarizeLinks implements LinkSummarizer offline (план §4.7):
// детерминированно все Summary="" — offline ссылки не саммаризуются (без
// сети). Контракт покрытия: ровно по одному LinkSummary на страницу,
// Index 1..n. Два вызова на одном входе байт-в-байт одинаковы.
func (o *Offline) SummarizeLinks(ctx context.Context, pages []LinkPage) ([]LinkSummary, error) {
	_ = ctx
	out := make([]LinkSummary, len(pages))
	for i := range out {
		out[i] = LinkSummary{Index: i + 1} // Summary="" — деградация по определению
	}
	return out, nil
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

// AggregateTopic реализует TopicAggregator offline (план §4.7, вызов C):
// headline темы — первые ≤3 строки «- <саммари сообщения>» из ВСЕХ топиков
// (порядок среза входа, паттерн Offline.SummarizeTopic); саммари каждого
// топика — первые ≤3 строки его собственного списка. Sections содержат по
// одной строке на топик входа (пустые саммари дают Summary="" — L1 деградирует
// на строку без саммари). Детерминированно: два вызова на одном входе
// байт-в-байт одинаковы (нет сети, random, time.Now, map-итераций).
func (o *Offline) AggregateTopic(ctx context.Context, topicName string, sources []SourceBlock) (TopicAggregate, error) {
	_ = ctx
	_ = topicName
	var all []string
	for _, s := range sources {
		all = append(all, s.Summaries...)
	}
	agg := TopicAggregate{
		Headline: strings.Join(firstSummaryLines(all, aggregateFallbackLines), "\n"),
	}
	for _, s := range sources {
		agg.Sections = append(agg.Sections, TopicHeadline{
			SourceID: s.SourceID,
			Summary:  strings.Join(firstSummaryLines(s.Summaries, aggregateFallbackLines), "\n"),
		})
	}
	return agg, nil
}

// AggregateOverall реализует OverallAggregator offline (план §4.7, вызов D):
// выпуск — список «- <имя темы>: N материалов» (без саммари — offline не из
// чего его строить). Пустое имя темы → «Без темы» (защита от пустой строки
// списка). Детерминированно: два вызова на одном входе байт-в-байт одинаковы.
func (o *Offline) AggregateOverall(ctx context.Context, topics []OverallTopic) (string, error) {
	_ = ctx
	lines := make([]string, 0, len(topics))
	for _, t := range topics {
		name := strings.TrimSpace(t.Name)
		if name == "" {
			name = "Без темы"
		}
		lines = append(lines, "- "+name+": "+itoa(t.ItemCount)+" материалов")
	}
	return strings.Join(lines, "\n"), nil
}
