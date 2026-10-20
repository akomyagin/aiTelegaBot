// Topic renders of Stage 8 C (plan §C, §4.11a): the per-topic L1 header
// stored in digests.body, the shared L1 view (reused by Stage 8 D — the
// signature is frozen), the run table of contents (TOC) and its buttons.
// All dynamic text goes through telegram.EscapeHTML (§4.8); markup is created
// only by the render itself. The legacy Render (pipeline.go) stays alive
// until C2.

package digest

import (
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/akomyagin/aiTelegaBot/internal/storage"
	"github.com/akomyagin/aiTelegaBot/internal/telegram"
)

// TOCEntry is one line of the digest table of contents: a topic of the run,
// its persisted digest id (callback "T:<DigestID>") and the number of fresh
// items.
type TOCEntry struct {
	DigestID  int64
	TopicName string
	ItemCount int
}

const (
	// topicTitleRunes caps each item title in the L1 view.
	topicTitleRunes = 120
	// maxBodyRunes is Telegram's message-text limit (ParseMode HTML).
	maxBodyRunes = 4096
	// topicOpenPrefix/topicOpenBudget: the Bot API button-text limit is 64
	// BYTES (урок A2 H1) — the dynamic part of «Открыть: <тема>» is cut to the
	// byte budget on a rune boundary with a "…" suffix (TruncateButtonLabelTo).
	topicOpenPrefix = "Открыть: "
	topicOpenBudget = 64 - len(topicOpenPrefix) - len("…")
	// topicBodyFallbackTitles caps the fallback title bullets of
	// RenderTopicBody (plan C: first 10 items when the headline is empty).
	topicBodyFallbackTitles = 10
)

// RenderTopicBody renders the per-topic L1 header stored verbatim in
// digests.body at run time (ready HTML under ParseMode). Format (golden):
//
//	📋 <topicName> — <02 Jan 2006>
//
//	<headline>            ← при пустом headline: строки "• <title>" первых 10
//	                         fallbackTitles (fallback-ветка, план C)
//
//	— N материалов        (склонения нет — стиль старого Render)
//
// Отклонение C1 (задокументировано в отчёте): сигнатура плана принимает
// itemCount, а не items, поэтому заголовки fallback-ветки передаются
// вариативным хвостом — вызов шага 5b модели доставки
// RenderTopicBody(topicName, ts.Headline, len(items), now) компилируется без
// изменений; C2 SHOULD передать до 10 заголовков элементов, чтобы ветка
// работала в проде.
func RenderTopicBody(topicName, headline string, itemCount int, now time.Time, fallbackTitles ...string) string {
	middle := strings.TrimSpace(headline)
	if middle == "" {
		lines := make([]string, 0, topicBodyFallbackTitles)
		for _, title := range fallbackTitles {
			if len(lines) >= topicBodyFallbackTitles {
				break
			}
			title = strings.TrimSpace(title)
			if title == "" {
				continue
			}
			lines = append(lines, "• "+telegram.EscapeHTML(title))
		}
		middle = strings.Join(lines, "\n")
	} else {
		middle = telegram.EscapeHTML(middle)
	}

	var b strings.Builder
	b.WriteString("📋 ")
	b.WriteString(telegram.EscapeHTML(topicName))
	b.WriteString(" — ")
	b.WriteString(now.Format("02 Jan 2006"))
	if middle != "" {
		b.WriteString("\n\n")
		b.WriteString(middle)
	}
	b.WriteString("\n\n— ")
	b.WriteString(strconv.Itoa(itemCount))
	b.WriteString(" материалов")
	return b.String()
}

// RenderTopicL1 renders the L1 topic view: the stored body AS IS (already
// HTML — never re-escape it) plus one "• <title>" line per digest item.
// Shared helper for Stage 8 D («L1:<digestID>» restore MUST reuse it — the
// signature is frozen by plan C). Empty items (crash case delivered=0) →
// body only. Guard: over 4096 runes the items block is cut from the tail on
// a rune boundary with an «…и ещё K» marker; if it still does not fit, the
// body itself is cut ("…"). No L1 pagination (revision trigger: regularly
// cut topics of >~50 items).
func RenderTopicL1(body string, items []storage.DigestItem) string {
	if len(items) == 0 {
		return body
	}
	lines := make([]string, 0, len(items))
	for _, it := range items {
		lines = append(lines, "• "+telegram.EscapeHTML(truncateRunes(it.Title, topicTitleRunes)))
	}
	for dropped := 0; dropped <= len(lines); dropped++ {
		block := strings.Join(lines[:len(lines)-dropped], "\n")
		if dropped > 0 {
			marker := "…и ещё " + strconv.Itoa(dropped)
			if block != "" {
				block += "\n" + marker
			} else {
				block = marker
			}
		}
		full := body
		if block != "" {
			full = body + "\n\n" + block
		}
		if utf8.RuneCountInString(full) <= maxBodyRunes {
			return full
		}
	}
	// Even the body alone is too long: cut the body on a rune boundary.
	r := []rune(body)
	return string(r[:maxBodyRunes-1]) + "…"
}

// RenderTOC renders the run's table of contents (golden):
//
//	📋 Дайджест — <date>   ← date="" → строка без «— <date>»
//
//	Тем: N
//
//	• <b>TopicName</b> — <ItemCount> материалов   (по теме, порядок entries)
//
// At run time date = now.Format("02 Jan 2006"); on restore (C2/D) — the first
// ListRunDigests row's CreatedAt parsed as RFC3339 and formatted the same way
// (parse error → date="").
func RenderTOC(entries []TOCEntry, date string) string {
	var b strings.Builder
	b.WriteString("📋 Дайджест")
	if date != "" {
		b.WriteString(" — ")
		b.WriteString(date)
	}
	b.WriteString("\n\nТем: ")
	b.WriteString(strconv.Itoa(len(entries)))
	if len(entries) > 0 {
		b.WriteString("\n")
	}
	for _, e := range entries {
		b.WriteString("\n• <b>")
		b.WriteString(telegram.EscapeHTML(e.TopicName))
		b.WriteString("</b> — ")
		b.WriteString(strconv.Itoa(e.ItemCount))
		b.WriteString(" материалов")
	}
	return b.String()
}

// TOCButtons builds the TOC inline keyboard — one button per row:
// «Открыть: <тема>» with callback data "T:<digestID>" (plan §4.4). The label
// is cut to the 64-BYTE Bot API limit on a rune boundary (урок A2 H1).
func TOCButtons(entries []TOCEntry) [][]telegram.InlineButton {
	rows := make([][]telegram.InlineButton, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, []telegram.InlineButton{{
			Text: topicOpenPrefix + telegram.TruncateButtonLabelTo(e.TopicName, topicOpenBudget),
			Data: "T:" + strconv.FormatInt(e.DigestID, 10),
		}})
	}
	return rows
}

// truncateRunes returns s cut to at most n runes (local digest helper — the
// llm and storage copies are unexported; plan C accepts the duplication).
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
