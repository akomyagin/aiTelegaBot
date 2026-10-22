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
	// topicHeadlineRunes caps the headline stored in digests.body (review C1
	// M2). Matches the llm JSON-path cap; the Summarize-fallback and offline
	// headlines pass through RenderTopicBody, which enforces it for all.
	topicHeadlineRunes = 800
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
	// Review C1 M2: digests.body feeds the L1 topic view whose guard cuts (or, on
	// the pre-fix code, panicked near) Telegram's 4096-rune limit — cap the
	// dynamic parts HERE so the stored body stays well below it: headline to
	// topicHeadlineRunes (the llm JSON path caps at 800 too, but the
	// Summarize-fallback and offline paths do not), fallback titles to
	// topicTitleRunes each (same cap as the L1 item lines).
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
			lines = append(lines, "• "+telegram.EscapeHTML(truncateRunes(title, topicTitleRunes)))
		}
		middle = strings.Join(lines, "\n")
	} else {
		middle = telegram.EscapeHTML(truncateRunes(middle, topicHeadlineRunes))
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

// RenderTOC renders the run's table of contents (golden):
//
//	📋 Дайджест — <date>   ← date="" → строка без «— <date>»
//
//	<overall>             ← общее саммари выпуска (вызов D / runs.summary);
//	                        пусто/не передано → блок ОТСУТСТВУЕТ — legacy-
//	                        прогоны и поведение Этапа 8/9A–9C байт-в-байт
//
//	Тем: N
//
//	• <b>TopicName</b> — <ItemCount> материалов   (по теме, порядок entries)
//
// At run time date = now.Format("02 Jan 2006"); on restore (C2/D) — the first
// ListRunDigests row's CreatedAt parsed as RFC3339 and formatted the same way
// (parse error → date="").
//
// Сигнатура 9D-1 (ОТКЛОНЕНИЕ от буквального «параметр overall string» плана,
// зафиксировано в отчёте): overall — вариативный хвост. internal/app/
// digest_nav.go (вне зоны 9D-1, трогать запрещено) и существующие тесты зовут
// RenderTOC с двумя аргументами; вариативность сохраняет их компиляцию и
// байт-в-байт прежний TOC без overall. 9D-2 начнёт передавать overall явно
// (run-пайплайн из runs.summary, viewer — из GetRunSummary) и может сделать
// параметр обязательным при переподключении viewer'а.
func RenderTOC(entries []TOCEntry, date string, overall ...string) string {
	var b strings.Builder
	b.WriteString("📋 Дайджест")
	if date != "" {
		b.WriteString(" — ")
		// date comes from time.Format (run time) or a parsed CreatedAt
		// (restore, C2/D) — escaped defensively (review C1 L4).
		b.WriteString(telegram.EscapeHTML(date))
	}
	if len(overall) > 0 {
		if o := strings.TrimSpace(overall[0]); o != "" {
			b.WriteString("\n\n")
			b.WriteString(telegram.EscapeHTML(truncateRunes(o, tocOverallRunes)))
		}
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
