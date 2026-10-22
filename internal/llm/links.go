// Виды внешних ссылок + саммари статей (Этап 9B, вызовы E и F плана §4.8).
//
// Вызов E (ClassifyLinks): классификация ссылок ДО fetch (план §4.3) — вход
// тексты сообщений темы и их URL, ответ строго JSON
// {"links":[{"msg":1,"link":2,"kind":"article"}]}. Fetch'ятся только
// article|other («неизвестное считаем статьёй»); course|ad → строка в
// link_cache без fetch.
// Вызов F (SummarizeLinks): саммари fetched-текстов (Title + Text ≤1500 рун
// на ссылку), ответ {"items":[{"index":1,"summary":"…"}]}.
//
// Оба вызова: строгий JSON, fence-strip, ровно ОДИН повтор при мусоре —
// паттерн SummarizeTopic; мусор после двух запросов → error (в отличие от
// вызова A — БЕЗ fallback-результата: деградирует вызывающий, план §4.3 —
// ссылки остаются в item_links без саммари, сообщение не теряется).
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Виды ссылок (план §4.3). Эти же строки пишутся в БД (link_cache.kind,
// item_links.kind) и ожидаются в JSON-ответе вызова E. Значение
// "unavailable" — только storage/fetcher-происхождения (ошибка скачивания),
// LLM его не возвращает.
const (
	LinkKindArticle = "article" // статья/лонгрид — fetch'им, саммари (вызов F)
	LinkKindCourse  = "course"  // обучающий КУРС — не fetch'им
	LinkKindAd      = "ad"      // реклама — не fetch'им
	LinkKindOther   = "other"   // прочее/неизвестное — считаем статьёй, fetch'им
)

// MsgLinks — вход вызова E по одному сообщению: текст сообщения и извлечённые
// из него внешние URL (pipeline, часть 3, строит их через extractURLs —
// порядок появления, ≤LinkPerMessageMax). Ссылки нумеруются 1-based внутри
// сообщения, сообщения — 1-based в срезе входа.
type MsgLinks struct {
	Text string
	URLs []string
}

// LinkKind — результат вызова E по одной паре (сообщение, ссылка): Msg —
// 1-based номер сообщения в срезе входа, Link — 1-based номер URL внутри
// MsgLinks.URLs, Kind — одна из констант LinkKind*.
type LinkKind struct {
	Msg  int
	Link int
	Kind string
}

// LinkPage — вход вызова F по одной ссылке: страница из linkfetch.Fetcher
// (URL для логов/валидации, Title и Text уходят в промпт).
type LinkPage struct {
	URL   string
	Title string
	Text  string
}

// LinkSummary — результат вызова F по одной странице. Index — 1-based
// порядковый номер страницы в срезе, переданном SummarizeLinks. Summary=""
// означает «саммари нет» (LLM пропустила индекс или вернула пустоту) —
// деградация, а не ошибка (план §4.3).
type LinkSummary struct {
	Index   int
	Summary string
}

// LinkClassifier — вызов E плана §4.8: виды ссылок батчем на тему,
// классификация ДО fetch (§4.3). Реализуют и сетевой *Client, и *Offline
// (интерфейс на второй реализации — правило SKILL.md).
//
// КОНТРАКТ ПОКРЫТИЯ: при err == nil результат содержит РОВНО по одному
// LinkKind на каждую пару (msg,link) входа, в порядке входа (msg 1..n,
// link 1..len(URLs)) — отсутствующая в ответе LLM пара получает kind=other
// детерминированно («неизвестное считаем статьёй», §4.3).
// Ошибка транспорта/API → error немедленно; мусорный ответ → ровно один
// повтор, затем error (деградирует вызывающий, не этот слой).
type LinkClassifier interface {
	ClassifyLinks(ctx context.Context, msgs []MsgLinks) ([]LinkKind, error)
}

// LinkSummarizer — вызов F плана §4.8: саммари fetched-статей батчем на
// тему. Реализуют и сетевой *Client, и *Offline.
//
// КОНТРАКТ ПОКРЫТИЯ: при err == nil результат содержит РОВНО по одному
// LinkSummary на страницу, Index 1..n в порядке входа; отсутствующий в
// ответе индекс → Summary="" (НЕ ошибка). Семантика ошибок — как у
// LinkClassifier: транспорт → error; двойной мусор → error (ссылки останутся
// без саммари — §4.3, сообщение остаётся в дайджесте).
type LinkSummarizer interface {
	SummarizeLinks(ctx context.Context, pages []LinkPage) ([]LinkSummary, error)
}

var (
	_ LinkClassifier = (*Client)(nil)
	_ LinkClassifier = (*Offline)(nil)
	_ LinkSummarizer = (*Client)(nil)
	_ LinkSummarizer = (*Offline)(nil)
)

// linkTextRunes ограничивает текст каждого сообщения в промпте вызова E.
// РЕШЕНИЕ ДИСПЕТЧЕРА (план §4.8 говорит «тексты сообщений» без лимита):
// защитная обрезка ≤200 рун — тот же бюджет, что у classifyTextRunes вызова
// A: полный текст сообщения LLM для вида ссылки не нужен, а патологический
// пост не раздувает промпт.
const linkTextRunes = 200

// linkSummaryTextRunes — бюджет fetched-текста одной страницы в промпте
// вызова F (план §4.8: ≤1500 рун на ссылку).
const linkSummaryTextRunes = 1500

const linkClassifySystemPrompt = `Ты — ассистент Telegram-бота, определяющий вид каждой внешней ссылки в сообщениях дайджеста.
Ответь СТРОГО одним JSON-объектом вида {"links":[{"msg":1,"link":1,"kind":"article"}]} без пояснений и без markdown-обёрток.
Правила:
- links — ровно по одному объекту на каждую пару (сообщение, ссылка): msg — номер сообщения из списка (начиная с 1), link — номер ссылки внутри этого сообщения (начиная с 1);
- kind — ровно одна из строк: article (статья/лонгрид), course (обучающий КУРС), ad (реклама/продвижение), other (прочее или неизвестно);
- вид определяй по тексту сообщения и URL; если определить нельзя — other.`

// buildLinkClassifyPrompt строит сообщения чата для вызова E — паттерн
// buildClassifyPrompt: детерминированно, порядок входа сохранён (никаких
// map-итераций), текст сообщения ≤linkTextRunes, ссылки нумерованы 1-based
// внутри сообщения.
func buildLinkClassifyPrompt(msgs []MsgLinks) []message {
	var b strings.Builder
	b.WriteString("Сообщения и их ссылки:")
	if len(msgs) == 0 {
		b.WriteString("\n(недоступны)")
	}
	for i, m := range msgs {
		b.WriteString("\n")
		b.WriteString(itoa(i + 1))
		b.WriteString(". ")
		b.WriteString(truncateRunes(m.Text, linkTextRunes))
		for j, u := range m.URLs {
			b.WriteString("\n  [")
			b.WriteString(itoa(j + 1))
			b.WriteString("] ")
			b.WriteString(u)
		}
	}
	return []message{
		{Role: "system", Content: linkClassifySystemPrompt},
		{Role: "user", Content: b.String()},
	}
}

// normalizeLinkKind приводит ответ LLM к константе вида §4.3: регистр и края
// игнорируются; всё неопознанное → other («неизвестное считаем статьёй»).
func normalizeLinkKind(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case LinkKindArticle:
		return LinkKindArticle
	case LinkKindCourse:
		return LinkKindCourse
	case LinkKindAd:
		return LinkKindAd
	default:
		return LinkKindOther
	}
}

// allLinkPairs перечисляет ВСЕ пары (msg,link) входа детерминированно —
// порядок входа, msg 1..n, link 1..len(URLs), — с kind=other. Общий источник
// предзаполнения парсера вызова E (отсутствующая в ответе пара → other) и
// Offline.ClassifyLinks (контракт покрытия LinkClassifier).
func allLinkPairs(msgs []MsgLinks) []LinkKind {
	total := 0
	for _, m := range msgs {
		total += len(m.URLs)
	}
	out := make([]LinkKind, 0, total)
	for i, m := range msgs {
		for j := range m.URLs {
			out = append(out, LinkKind{Msg: i + 1, Link: j + 1, Kind: LinkKindOther})
		}
	}
	return out
}

// parseLinksResponse разбирает ответ вызова E после stripCodeFence — паттерн
// parseClassifyResponse. Валидация: msg/link вне диапазона → отброшены; дубль
// пары → первый выигрыш; неизвестный kind → other (normalizeLinkKind);
// ОТСУТСТВУЮЩАЯ пара → kind=other (предзаполнение, детерминированно).
// Пустой links при непустом входе — мусор (решение 9A №8 по {"items":[]}
// распространено на вызов E): вызывающий делает ровно один повтор, затем
// error.
func parseLinksResponse(content string, msgs []MsgLinks) ([]LinkKind, error) {
	var resp struct {
		Links []struct {
			Msg  int    `json:"msg"`
			Link int    `json:"link"`
			Kind string `json:"kind"`
		} `json:"links"`
	}
	if err := json.Unmarshal([]byte(stripCodeFence(content)), &resp); err != nil {
		return nil, fmt.Errorf("llm: parse links response: %w", err)
	}
	pairs := allLinkPairs(msgs)
	if len(pairs) > 0 && len(resp.Links) == 0 {
		return nil, fmt.Errorf("llm: parse links response: empty links array")
	}
	pos := make(map[[2]int]int, len(pairs))
	for i, p := range pairs {
		pos[[2]int{p.Msg, p.Link}] = i
	}
	filled := make(map[int]struct{}, len(resp.Links))
	for _, l := range resp.Links {
		if l.Msg < 1 || l.Msg > len(msgs) {
			continue
		}
		if l.Link < 1 || l.Link > len(msgs[l.Msg-1].URLs) {
			continue
		}
		i := pos[[2]int{l.Msg, l.Link}]
		if _, dup := filled[i]; dup {
			continue // дубль пары — первый выигрыш
		}
		filled[i] = struct{}{}
		pairs[i].Kind = normalizeLinkKind(l.Kind)
	}
	return pairs, nil
}

const linkSummarySystemPrompt = `Ты — ассистент Telegram-бота, готовящий краткие саммари статей по их извлечённым текстам.
Ответь СТРОГО одним JSON-объектом вида {"items":[{"index":1,"summary":"..."}]} без пояснений и без markdown-обёрток.
Правила:
- items — ровно по одному объекту на каждую статью списка: index — порядковый номер статьи (начиная с 1);
- summary — 1–2 предложения сути на русском, plain text без HTML и без markdown;
- если текст не читается или его недостаточно — верни пустой summary для этого index.`

// buildLinkSummaryPrompt строит сообщения чата для вызова F — паттерн
// buildTopicDigestPrompt: детерминированно, порядок входа сохранён,
// нумерованные страницы (Title + Text ≤linkSummaryTextRunes, обрезка на
// границе рун).
func buildLinkSummaryPrompt(pages []LinkPage) []message {
	var b strings.Builder
	b.WriteString("Статьи:")
	if len(pages) == 0 {
		b.WriteString("\n(недоступны)")
	}
	for i, p := range pages {
		b.WriteString("\n")
		b.WriteString(itoa(i + 1))
		b.WriteString(". ")
		b.WriteString(p.Title)
		if text := truncateRunes(p.Text, linkSummaryTextRunes); text != "" {
			b.WriteString("\n")
			b.WriteString(text)
		}
	}
	return []message{
		{Role: "system", Content: linkSummarySystemPrompt},
		{Role: "user", Content: b.String()},
	}
}

// parseLinkSummaries разбирает ответ вызова F после stripCodeFence — паттерн
// parseTopicSummary: index вне 1..n → отброшен, дубль → первый выигрыш,
// ОТСУТСТВУЮЩИЙ index → Summary="" (не ошибка); пустой summary от LLM
// остаётся "" (не fallback — деградация вызывающего). Пустой items при
// непустом входе — мусор (решение 9A №8): один повтор, затем error.
func parseLinkSummaries(content string, n int) ([]LinkSummary, error) {
	var resp struct {
		Items []struct {
			Index   int    `json:"index"`
			Summary string `json:"summary"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(stripCodeFence(content)), &resp); err != nil {
		return nil, fmt.Errorf("llm: parse link summaries: %w", err)
	}
	if n > 0 && len(resp.Items) == 0 {
		return nil, fmt.Errorf("llm: parse link summaries: empty items array")
	}
	out := make([]LinkSummary, n)
	for i := range out {
		out[i] = LinkSummary{Index: i + 1} // отсутствующий индекс → Summary=""
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
		out[it.Index-1].Summary = it.Summary
	}
	return out, nil
}
