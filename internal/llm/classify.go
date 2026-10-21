// Классификация + саммари сообщений (Этап 9, вызов A плана §4.8).
//
// Один вызов — батч на тему (чанкование ≤60 элементов на HTTP-запрос — внутри
// Client.ClassifySummarize). Ответ LLM — строго JSON
// {"items":[{"index":1,"class":"news","summary":"…"}]}; парсер-валидатор —
// паттерн parseTopicSummary (fence-strip, index вне 1..n → отброшен, дубли →
// первый выигрыш, неизвестный class → other).
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Классы материалов (план §4.1). Эти же строки пишутся в БД
// (message_state.class) и ожидаются в JSON-ответе LLM.
const (
	ClassNews     = "news"     // новость
	ClassArticle  = "article"  // статья / лонгрид
	ClassTutorial = "tutorial" // обучающий материал (не курс)
	ClassCourse   = "course"   // обучающий КУРС (одна фраза, ссылки не fetch'им)
	ClassOpinion  = "opinion"  // высказывание/мнение
	ClassMedia    = "media"    // медиа-пост (картинка/видео — основное содержание)
	ClassAd       = "ad"       // реклама (pipeline выбрасывает полностью)
	ClassOther    = "other"    // прочее (и любая деградация)
)

// ClassifyItem — входной элемент вызова A: только то, что нужно LLM для
// классификации и саммари. Pipeline (часть 3) строит срез из feed.Item:
// Title ← it.Title, Text ← it.Text.
type ClassifyItem struct {
	Title string
	Text  string
}

// ItemClassSummary — результат по одному элементу. Index — 1-based порядковый
// номер в срезе, переданном ClassifySummarize: чанкование внутреннее для
// реализации, индексы для вызывающего всегда глобальные.
type ItemClassSummary struct {
	Index   int
	Class   string
	Summary string
}

// ClassifyResult — ответ вызова A. Items всегда покрывает ВСЕ элементы 1..n
// в порядке входа. Degraded параллелен Items: Degraded[i] == true означает,
// что Items[i] получен детерминированной деградацией (class=other,
// summary=extractLead), а не ответом LLM. Семантика для вызывающего (часть 3):
// деградированному элементу НЕ писать status=done в message_state — следующий
// прогон переспросит LLM (план §4.8, строки 480–484). Источники деградации:
//   - двойной мусор в ответе на чанк (fallback — тема НЕ пропускается);
//   - индекс, ОТСУТСТВУЮЩИЙ в валидном JSON-ответе (решение диспетчера:
//     детерминированная деградация, не ошибка — валидный JSON не ретраится);
//   - offline-режим (выход по определению не LLM-качества: если позже появится
//     API-ключ, элементы не должны остаться навсегда с class=other).
type ClassifyResult struct {
	Items    []ItemClassSummary
	Degraded []bool
}

// ClassifySummarizer — вызов A плана §4.8: классификация + саммари на
// сообщение. Реализуют и сетевой *Client, и *Offline (интерфейс введён на
// второй реализации — правило SKILL.md соблюдено). Ошибка транспорта →
// error (тема в pipeline пропускается целиком, план §4.9); мусорный ответ —
// НЕ ошибка (fallback через Degraded).
//
// При err != nil результат МОЖЕТ содержать частичные данные — элементы
// успешно обработанных чанков (покрывают префикс входа 1..k, индексы
// глобальные). Вызывающий ОБЯЗАН закешировать не-Degraded часть ДО обработки
// сбоя, чтобы следующий прогон не переспрашивал LLM по уже готовым элементам.
type ClassifySummarizer interface {
	ClassifySummarize(ctx context.Context, items []ClassifyItem) (ClassifyResult, error)
}

var (
	_ ClassifySummarizer = (*Client)(nil)
	_ ClassifySummarizer = (*Offline)(nil)
)

// classifyTextRunes ограничивает текст каждого сообщения в промпте вызова A
// (план §4.8: text ≤200 рун).
const classifyTextRunes = 200

// classifyChunkSize — потолок элементов на один HTTP-запрос (план, строки
// 555–557: решение планировщика — JSON-ответ надёжнее короче).
const classifyChunkSize = 60

const classifySystemPrompt = `Ты — ассистент Telegram-бота, классифицирующий сообщения дайджеста по теме и готовящий их краткие саммари.
Ответь СТРОГО одним JSON-объектом вида {"items":[{"index":1,"class":"news","summary":"..."}]} без пояснений и без markdown-обёрток.
Правила:
- items — ровно по одному объекту на каждый материал списка: index — порядковый номер материала из списка (начиная с 1);
- class — ровно одна из строк: news (новость), article (статья/лонгрид), tutorial (обучающий материал, не курс), course (обучающий КУРС), opinion (высказывание/мнение), media (медиа-пост, картинка/видео — основное содержание), ad (реклама/продвижение), other (прочее);
- summary — 1–2 предложения сути на русском, plain text без HTML и без markdown;
- для class=course summary — строго одна фраза вида "Курс: <название> — <тема>", без упоминания ссылок и URL;
- если материал не подходит ни под один класс — other.`

// buildClassifyPrompt строит сообщения чата для вызова A — паттерн
// buildTopicDigestPrompt: детерминированно, порядок входа сохранён,
// нумерованные элементы (Title + Text ≤ classifyTextRunes).
func buildClassifyPrompt(items []ClassifyItem) []message {
	var b strings.Builder
	b.WriteString("Материалы:")
	if len(items) == 0 {
		b.WriteString("\n(недоступны)")
	}
	for i, it := range items {
		b.WriteString("\n")
		b.WriteString(itoa(i + 1))
		b.WriteString(". ")
		b.WriteString(truncateRunes(it.Title, classifyTextRunes)) // I2: title тоже ≤200 рун
		if text := truncateRunes(it.Text, classifyTextRunes); text != "" {
			b.WriteString("\n")
			b.WriteString(text)
		}
	}
	return []message{
		{Role: "system", Content: classifySystemPrompt},
		{Role: "user", Content: b.String()},
	}
}

// normalizeClass приводит ответ LLM к константе класса §4.1: регистр и края
// игнорируются; всё неопознанное → other (план: неизвестный class → other).
func normalizeClass(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case ClassNews:
		return ClassNews
	case ClassArticle:
		return ClassArticle
	case ClassTutorial:
		return ClassTutorial
	case ClassCourse:
		return ClassCourse
	case ClassOpinion:
		return ClassOpinion
	case ClassMedia:
		return ClassMedia
	case ClassAd:
		return ClassAd
	default:
		return ClassOther
	}
}

// fallbackSummary — детерминированное саммари деградации: extractLead(Text),
// при пустом Text — Title (тот же паттерн, что в Offline.SummarizeTopic).
func fallbackSummary(it ClassifyItem) string {
	if lead := extractLead(it.Text); lead != "" {
		return lead
	}
	return it.Title
}

// degradedClassifyResult — детерминированная деградация на весь срез: все
// class=other, summary=fallbackSummary, Degraded=true. Общий источник для
// fallback'а двойного мусора (Client) и offline-режима (Offline), а также
// предзаполнение отсутствующих индексов в parseClassifyResponse.
func degradedClassifyResult(items []ClassifyItem) ClassifyResult {
	res := ClassifyResult{
		Items:    make([]ItemClassSummary, len(items)),
		Degraded: make([]bool, len(items)),
	}
	for i, it := range items {
		res.Items[i] = ItemClassSummary{Index: i + 1, Class: ClassOther, Summary: fallbackSummary(it)}
		res.Degraded[i] = true
	}
	return res
}

// parseClassifyResponse разбирает ответ вызова A после stripCodeFence —
// паттерн parseTopicSummary. Валидация: index вне 1..len(items) → отброшен;
// дубли index → первый выигрыш; неизвестный class → other (normalizeClass).
// РЕШЕНИЕ ДИСПЕТЧЕРА (кейс не покрыт планом явно): индексы, отсутствующие в
// валидном ответе, получают class=other + summary=extractLead и Degraded=true
// — это детерминированная деградация, а не ошибка: валидный JSON не ретраится,
// тема не пропускается, а элемент без status=done переспрашивается следующим
// прогоном. Мусор (не JSON) → error: вызывающий делает ровно один повтор,
// затем полный fallback чанка (degradedClassifyResult).
func parseClassifyResponse(content string, items []ClassifyItem) (ClassifyResult, error) {
	var resp struct {
		Items []struct {
			Index   int    `json:"index"`
			Class   string `json:"class"`
			Summary string `json:"summary"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(stripCodeFence(content)), &resp); err != nil {
		return ClassifyResult{}, fmt.Errorf("llm: parse classify response: %w", err)
	}
	// Пустой items при непустом входе — мусорный ответ (L2): тот же путь, что
	// и не-JSON — ровно один повтор, затем fallback degradedClassifyResult.
	if len(items) > 0 && len(resp.Items) == 0 {
		return ClassifyResult{}, fmt.Errorf("llm: parse classify response: empty items array")
	}
	// Предзаполнение: отсутствующие индексы уже деградированы.
	res := degradedClassifyResult(items)
	filled := make([]bool, len(items))
	for _, it := range resp.Items {
		if it.Index < 1 || it.Index > len(items) {
			continue
		}
		summary := strings.TrimSpace(it.Summary)
		if summary == "" {
			// L5: валидный индекс, но пустое summary — НЕ заполняем: элемент
			// остаётся Degraded=true с fallbackSummary; последующий дубль
			// индекса с непустым summary ещё может заполнить.
			continue
		}
		if filled[it.Index-1] { // дубль — первый выигрыш
			continue
		}
		filled[it.Index-1] = true
		res.Items[it.Index-1] = ItemClassSummary{
			Index:   it.Index,
			Class:   normalizeClass(it.Class),
			Summary: summary,
		}
		res.Degraded[it.Index-1] = false
	}
	return res, nil
}
