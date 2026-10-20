package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
)

// ItemSummary is the LLM's one-line summary of a single item. Index is the
// 1-based position of the item in the slice passed to SummarizeTopic.
type ItemSummary struct {
	Index   int
	Summary string
}

// TopicSummary is the structured per-topic digest (plan §4.10). Headline is
// plain text — the render layer escapes it, NOT here (§4.8). Items may be
// empty: the L2 view then degrades to item titles instead of failing.
type TopicSummary struct {
	Headline string
	Items    []ItemSummary
}

// TopicSummarizer summarizes one topic's items into a TopicSummary. Both the
// networked Client and Offline implement it (second implementation → the
// interface is justified by the SKILL.md rule). Plan §4.10 / Stage 8 C.
type TopicSummarizer interface {
	SummarizeTopic(ctx context.Context, topicName string, items []feed.Item) (TopicSummary, error)
}

// maxTopicHeadlineRunes caps the headline (plan C, planner decision: защита
// от body >4096 в L1-рендере).
const maxTopicHeadlineRunes = 800

// topicDigestTextRunes caps each item's body in the prompt — the same budget
// as the topic-suggestion sample (topic.go).
const topicDigestTextRunes = 200

const topicDigestSystemPrompt = `Ты — ассистент Telegram-бота, готовящий сводку одного выпуска дайджеста по теме.
Ответь СТРОГО одним JSON-объектом вида {"headline":"...","items":[{"index":1,"summary":"..."}]} без пояснений и без markdown-обёрток.
Правила:
- headline — 2–3 предложения, краткая суть выпуска темы;
- items — ровно по одному объекту на каждый материал списка: index — порядковый номер материала из списка (начиная с 1), summary — 1–2 предложения сути;
- весь текст — plain text, без HTML и без markdown;
- язык — русский.`

// buildTopicDigestPrompt builds the chat messages for a topic-digest request.
// Deterministic for a given input (order preserved) — the buildTopicPrompt
// pattern from topic.go: topic name + numbered items (Title + Text cut to
// topicDigestTextRunes).
func buildTopicDigestPrompt(topicName string, items []feed.Item) []message {
	var b strings.Builder
	b.WriteString("Тема: ")
	b.WriteString(topicName)
	b.WriteString("\n\nМатериалы выпуска:")
	if len(items) == 0 {
		b.WriteString("\n(недоступны)")
	}
	for i, it := range items {
		b.WriteString("\n")
		b.WriteString(itoa(i + 1))
		b.WriteString(". ")
		b.WriteString(it.Title)
		if text := truncateRunes(it.Text, topicDigestTextRunes); text != "" {
			b.WriteString("\n")
			b.WriteString(text)
		}
	}
	return []message{
		{Role: "system", Content: topicDigestSystemPrompt},
		{Role: "user", Content: b.String()},
	}
}

// parseTopicSummary parses the strict-JSON answer after stripping optional
// ```-fences. n = len(items): entries with Index outside 1..n and duplicate
// Index values are dropped (first wins); missing indexes are NOT an error; an
// empty Headline is NOT an error (the render has a fallback branch). The
// headline is trimmed and cut to maxTopicHeadlineRunes.
func parseTopicSummary(content string, n int) (TopicSummary, error) {
	var resp struct {
		Headline string `json:"headline"`
		Items    []struct {
			Index   int    `json:"index"`
			Summary string `json:"summary"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(stripCodeFence(content)), &resp); err != nil {
		return TopicSummary{}, fmt.Errorf("llm: parse topic summary: %w", err)
	}
	ts := TopicSummary{
		Headline: truncateRunes(strings.TrimSpace(resp.Headline), maxTopicHeadlineRunes),
	}
	seen := make(map[int]struct{}, len(resp.Items))
	for _, it := range resp.Items {
		if it.Index < 1 || it.Index > n {
			continue
		}
		if _, dup := seen[it.Index]; dup {
			continue
		}
		seen[it.Index] = struct{}{}
		ts.Items = append(ts.Items, ItemSummary{Index: it.Index, Summary: it.Summary})
	}
	return ts, nil
}
