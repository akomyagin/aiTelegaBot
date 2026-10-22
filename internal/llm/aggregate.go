// Агрегация саммари (Этап 9D, вызовы C и D плана §4.8): headline темы +
// саммари топиков (источников) из саммари primary-сообщений — вызов C, один
// на тему; общее саммари выпуска из headline'ов тем — вызов D, один на прогон
// (хранится в runs.summary, показывается в RenderTOC и при восстановлении
// TOC:<runID>). Каждый уровень строится из саммари нижнего, НЕ из сырых
// текстов (§4.7 — экономия токенов).
//
// Оба вызова: строгий JSON, fence-strip, ровно ОДИН повтор при мусоре —
// паттерн SummarizeTopic; ошибка транспорта/API → error немедленно
// (doWithRetry уже повторил). В отличие от вызовов E/F, двойной мусор → НЕ
// ошибка, а детерминированный fallback (план, тест-кейс 1 подэтапа 9D):
// C — headline = первые ≤3 саммари списком «- …», sections пусты (L1
// деградирует на строки без саммари топиков); D — overall "" (L0 без блока
// общего саммари, §4.8.1 шаг 6 — вызывающий логирует и продолжает).
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// SourceBlock — вход вызова C по одному топику (источнику): саммари его
// primary-сообщений В ПОРЯДКЕ collect (§4.6/9D: никаких map-итераций в
// промпт). SourceID — id строки sources (0 = неизвестен), Name —
// отображаемое имя топика (подзаголовок в промпте и в L1-строке).
type SourceBlock struct {
	SourceID  int64
	Name      string
	Summaries []string
}

// TopicHeadline — результат вызова C по одному топику: саммари топика 1–2
// предложения (§4.7). SourceID соответствует SourceBlock.SourceID входа.
type TopicHeadline struct {
	SourceID int64
	Summary  string
}

// TopicAggregate — результат вызова C по теме: Headline — саммари темы 2–3
// предложения (в 9D-2 уходит в digests.body), Sections — ≤одной на топик в
// порядке ответа LLM; топики с пустым саммари в Sections ОТСУТСТВУЮТ
// (соответствующая L1-строка рендерится без саммари — деградация, не ошибка).
type TopicAggregate struct {
	Headline string
	Sections []TopicHeadline
}

// OverallTopic — вход вызова D по одной теме. Name/ItemCount нужны
// offline-деградации «- <имя темы>: N материалов» (§4.7), Headline — из
// вызова C (промпт *Client).
//
// ОТКЛОНЕНИЕ 9D-1 от сигнатуры брифа AggregateOverall([]string): строка
// выпуска offline-режима содержит имя темы и число материалов — из одних
// headline'ов они невыводимы, поэтому вход — структура. 9D-2 строит срез из
// ThemeEntry прогона (порядок тем = порядок TOC, детерминированно).
type OverallTopic struct {
	Name      string
	ItemCount int
	Headline  string
}

// TopicAggregator — вызов C плана §4.8 (1 на тему). Реализуют и сетевой
// *Client, и *Offline — интерфейс на второй реализации (правило SKILL.md).
//
// КОНТРАКТ: при err == nil Sections содержит не более одной TopicHeadline на
// топик входа (SourceID из sources), Headline может быть пустым только при
// пустом входе. Ошибка транспорта/API → error; двойной мусор → fallback БЕЗ
// ошибки (см. заголовок файла). Пустой вход → пустой результат без запроса.
type TopicAggregator interface {
	AggregateTopic(ctx context.Context, topicName string, sources []SourceBlock) (TopicAggregate, error)
}

// OverallAggregator — вызов D плана §4.8 (1 на прогон). Реализуют и сетевой
// *Client, и *Offline.
//
// КОНТРАКТ: возвращает общее саммари выпуска; "" означает «общего саммари
// нет» (L0 рендерится без блока overall — §4.8.1 шаг 6). Ошибка транспорта/
// API → error (вызывающий 9D-2 обрабатывает его log-only); двойной мусор →
// ("", nil). Пустой вход → ("", nil) без запроса.
type OverallAggregator interface {
	AggregateOverall(ctx context.Context, topics []OverallTopic) (string, error)
}

var (
	_ TopicAggregator   = (*Client)(nil)
	_ TopicAggregator   = (*Offline)(nil)
	_ OverallAggregator = (*Client)(nil)
	_ OverallAggregator = (*Offline)(nil)
)

const (
	// aggregateSummaryRunes — бюджет одного саммари сообщения в промпте C.
	// Защитная обрезка (сводный стиль 9A №7/9B linkTextRunes): саммари 9B уже
	// ≤600 рун, но патологическая строка не должна раздувать промпт.
	aggregateSummaryRunes = 600
	// overallHeadlineRunes — бюджет одного headline темы в промпте D
	// (согласовано с topicHeadlineRunes=800 — пределом digests.body).
	overallHeadlineRunes = 800
	// aggregateFallbackLines — сколько строк «- …» берётся в fallback
	// headline вызова C и в offline-headline (§4.7: первые ≤3 строки).
	aggregateFallbackLines = 3
)

const aggregateTopicSystemPrompt = `Ты — ассистент Telegram-бота, агрегирующий саммари сообщений дайджеста по теме.
Ответь СТРОГО одним JSON-объектом вида {"headline":"...","sections":[{"source_index":1,"summary":"..."}]} без пояснений и без markdown-обёрток.
Правила:
- headline — 2–3 предложения на русском, суммарно о главном в теме, plain text без HTML и без markdown;
- sections — ровно по одному объекту на каждый топик (источник) списка: source_index — номер топика из списка (начиная с 1);
- summary топика — 1–2 предложения на русском о главном из сообщений этого топика;
- если саммари топика недостаточно — верни пустой summary для этого source_index.`

const aggregateOverallSystemPrompt = `Ты — ассистент Telegram-бота, готовящий общее саммари выпуска дайджеста.
Ответь СТРОГО одним JSON-объектом вида {"summary":"..."} без пояснений и без markdown-обёрток.
Правила:
- summary — 3–5 предложений на русском о главном за весь выпуск, plain text без HTML и без markdown.`

// buildAggregateTopicPrompt строит сообщения чата для вызова C — паттерн
// buildLinkClassifyPrompt: детерминированно, порядок среза входа сохранён
// (никаких map-итераций), топики нумерованы 1-based, саммари внутри топика
// нумерованы 1-based и обрезаны до aggregateSummaryRunes.
func buildAggregateTopicPrompt(topicName string, sources []SourceBlock) []message {
	var b strings.Builder
	b.WriteString("Тема: ")
	b.WriteString(topicName)
	b.WriteString("\nТопики и саммари их сообщений:")
	if len(sources) == 0 {
		b.WriteString("\n(недоступны)")
	}
	for i, s := range sources {
		b.WriteString("\n")
		b.WriteString(itoa(i + 1))
		b.WriteString(". ")
		b.WriteString(s.Name)
		for j, sum := range s.Summaries {
			b.WriteString("\n  ")
			b.WriteString(itoa(j + 1))
			b.WriteString(") ")
			b.WriteString(truncateRunes(sum, aggregateSummaryRunes))
		}
	}
	return []message{
		{Role: "system", Content: aggregateTopicSystemPrompt},
		{Role: "user", Content: b.String()},
	}
}

// buildAggregateOverallPrompt строит сообщения чата для вызова D — тот же
// детерминированный паттерн: темы нумерованы 1-based, headline обрезан до
// overallHeadlineRunes.
func buildAggregateOverallPrompt(topics []OverallTopic) []message {
	var b strings.Builder
	b.WriteString("Темы выпуска и их заголовки:")
	if len(topics) == 0 {
		b.WriteString("\n(недоступны)")
	}
	for i, t := range topics {
		b.WriteString("\n")
		b.WriteString(itoa(i + 1))
		b.WriteString(". ")
		b.WriteString(t.Name)
		if h := strings.TrimSpace(t.Headline); h != "" {
			b.WriteString("\n")
			b.WriteString(truncateRunes(h, overallHeadlineRunes))
		}
	}
	return []message{
		{Role: "system", Content: aggregateOverallSystemPrompt},
		{Role: "user", Content: b.String()},
	}
}

// parseAggregateTopic разбирает ответ вызова C после stripCodeFence — паттерн
// parseLinksResponse: source_index вне 1..len(sources) → отброшен (план 9D,
// тест-кейс 1), дубль топика → первый выигрыш, пустой summary → секции нет
// (деградация L1-строки). Мусор (включая пустые headline И sections —
// обобщение решения 9A №8 про пустой массив) → error: вызывающий делает ровно
// один повтор, затем fallback degradedTopicAggregate.
func parseAggregateTopic(content string, sources []SourceBlock) (TopicAggregate, error) {
	var resp struct {
		Headline string `json:"headline"`
		Sections []struct {
			SourceIndex int    `json:"source_index"`
			Summary     string `json:"summary"`
		} `json:"sections"`
	}
	if err := json.Unmarshal([]byte(stripCodeFence(content)), &resp); err != nil {
		return TopicAggregate{}, fmt.Errorf("llm: parse aggregate response: %w", err)
	}
	headline := strings.TrimSpace(resp.Headline)
	if headline == "" && len(resp.Sections) == 0 {
		return TopicAggregate{}, fmt.Errorf("llm: parse aggregate response: empty headline and sections")
	}
	agg := TopicAggregate{Headline: headline}
	seen := make(map[int]struct{}, len(resp.Sections))
	for _, s := range resp.Sections {
		if s.SourceIndex < 1 || s.SourceIndex > len(sources) {
			continue
		}
		if _, dup := seen[s.SourceIndex]; dup {
			continue
		}
		seen[s.SourceIndex] = struct{}{}
		sum := strings.TrimSpace(s.Summary)
		if sum == "" {
			continue
		}
		agg.Sections = append(agg.Sections, TopicHeadline{
			SourceID: sources[s.SourceIndex-1].SourceID,
			Summary:  sum,
		})
	}
	return agg, nil
}

// parseOverallSummary разбирает ответ вызова D после stripCodeFence:
// {"summary":"…"}; непарсится или пустой summary → error (ровно один повтор,
// затем fallback overall "").
func parseOverallSummary(content string) (string, error) {
	var resp struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal([]byte(stripCodeFence(content)), &resp); err != nil {
		return "", fmt.Errorf("llm: parse overall summary: %w", err)
	}
	s := strings.TrimSpace(resp.Summary)
	if s == "" {
		return "", fmt.Errorf("llm: parse overall summary: empty summary")
	}
	return s, nil
}

// firstSummaryLines — первые ≤limit непустых саммари строками «- <саммари>»
// (детерминированно, порядок среза). Общая голова fallback'а вызова C и
// offline-headline темы (§4.7, паттерн Offline.SummarizeTopic).
func firstSummaryLines(summaries []string, limit int) []string {
	lines := make([]string, 0, limit)
	for _, s := range summaries {
		if len(lines) >= limit {
			break
		}
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		lines = append(lines, "- "+s)
	}
	return lines
}

// degradedTopicAggregate — детерминированный fallback вызова C при двойном
// мусоре (план 9D, тест-кейс 1): headline = первые ≤3 саммари всех топиков
// списком «- …» (порядок среза входа), sections пусты → L1 деградирует на
// строки без саммари топиков. Используется и Offline'ом (тот же формат).
func degradedTopicAggregate(sources []SourceBlock) TopicAggregate {
	var all []string
	for _, s := range sources {
		all = append(all, s.Summaries...)
	}
	return TopicAggregate{Headline: strings.Join(firstSummaryLines(all, aggregateFallbackLines), "\n")}
}
