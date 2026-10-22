package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
)

// TopicSuggester возвращает имя темы для нового источника: либо одно из
// existing (точное совпадение), либо новое короткое имя (≤40 рун).
// Пустая строка = «предложить нечего» (offline/ошибка — не фатально).
// Реализации: сетевой Client и Offline (вторая реализация → интерфейс
// оправдан правилом SKILL.md). План §4.10, Этап 8 B. Историческая справка:
// TopicSummarizer / llm/topicdigest.go удалены в 9D-2b — саммари топиков
// строит вызов C (AggregateTopic, llm/aggregate.go, §4.7).
type TopicSuggester interface {
	SuggestTopic(ctx context.Context, existing []string, sample []feed.Item) (string, error)
}

// maxTopicRunes — потолок имени темы (план: имя ≤40 рун).
const maxTopicRunes = 40

// maxTopicSampleTextRunes caps each sample item's body in the suggestion
// prompt — the model needs a hint, not the full text.
const maxTopicSampleTextRunes = 200

const topicSystemPrompt = `Ты — ассистент Telegram-бота, подбирающий короткую тему для нового источника контента.
Ответь СТРОГО одним JSON-объектом вида {"topic":"<имя темы>"} без пояснений и без markdown-обёрток.
Правила:
- имя темы — короткое (не более 40 символов), на русском;
- если источнику подходит одна из существующих тем — верни её точное имя;
- если предложить нечего — верни {"topic":""}.`

// buildTopicPrompt builds the chat messages for a topic-suggestion request.
// Deterministic for a given input (order preserved).
func buildTopicPrompt(existing []string, sample []feed.Item) []message {
	var b strings.Builder
	b.WriteString("Существующие темы:")
	if len(existing) == 0 {
		b.WriteString(" (нет)")
	}
	for _, name := range existing {
		b.WriteString("\n- ")
		b.WriteString(name)
	}
	b.WriteString("\n\nМатериалы нового источника:")
	if len(sample) == 0 {
		b.WriteString("\n(недоступны)")
	}
	for i, it := range sample {
		b.WriteString("\n")
		b.WriteString(itoa(i + 1))
		b.WriteString(". ")
		b.WriteString(it.Title)
		if it.URL != "" {
			b.WriteString("\nURL: ")
			b.WriteString(it.URL)
		}
		if text := truncateRunes(it.Text, maxTopicSampleTextRunes); text != "" {
			b.WriteString("\n")
			b.WriteString(text)
		}
	}
	return []message{
		{Role: "system", Content: topicSystemPrompt},
		{Role: "user", Content: b.String()},
	}
}

// parseTopicSuggestion parses the model answer: strict JSON {"topic":"..."}
// after stripping optional ```-fences. An empty/whitespace name means
// "предложить нечего" and is returned as ("", nil), not an error.
func parseTopicSuggestion(content string) (string, error) {
	var resp struct {
		Topic string `json:"topic"`
	}
	if err := json.Unmarshal([]byte(stripCodeFence(content)), &resp); err != nil {
		return "", fmt.Errorf("llm: parse topic suggestion: %w", err)
	}
	name := strings.TrimSpace(resp.Topic)
	// Решение (Этап 8 B): имя длиннее 40 рун обрезается по границе рун, а не
	// отклоняется — модель изредка превышает лимит, и укороченное имя полезнее
	// пользователю, чем отсутствующее предложение.
	return strings.TrimSpace(truncateRunes(name, maxTopicRunes)), nil
}

// stripCodeFence removes a markdown ``` fence (with optional language tag)
// that models sometimes add despite instructions.
func stripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	s = strings.TrimPrefix(s, "```")
	// Drop the language-tag line (e.g. "json") unless it already holds JSON.
	if i := strings.IndexByte(s, '\n'); i >= 0 && !strings.Contains(s[:i], "{") {
		s = s[i+1:]
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "```"))
}
