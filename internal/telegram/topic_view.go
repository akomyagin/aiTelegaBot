// Digest navigation views (Stage 8 C, план §C): callbacks «T:<digestID>»
// (open a topic's L1 view) and «TOC:<runID>» (back to the run's table of
// contents). Mechanics are A2/B one-to-one: every callback is acked first,
// inaccessible messages are ack-only, the result edits the pressed message in
// place. All data comes from SQLite through the DigestViewer seam — the
// telegram package keeps NO in-memory run snapshots, so views survive a bot
// restart (rest-семантика §4.3). Owner-check lives in the shared
// handleCallbackQuery contour and is not duplicated here.

package telegram

import (
	"context"
	"strconv"

	"github.com/go-telegram/bot/models"
)

// Callback data prefixes (plan §4.4): «TOC:» and «T:» do not intersect (the
// second byte differs) and do not conflict with ad:/dp:/ts:/tl:/tn: or the
// future L1:/L2:/L3:. Worst case "TOC:" + 19 digits = 23 bytes < 64. The
// dispatcher checks TOC: before T: for readability.
const (
	cbPrefixTOC       = "TOC:"
	cbPrefixTopicOpen = "T:"
)

// Stage 8 D drill-down prefixes (plan §4.4): «L1:<digestID>» (назад из L2 —
// alias T:-пути), «L2:<digestID>» (по источникам), «L3:<digestID>:<sourceID>»
// (элементы источника; sourceID=0 → общий список). Не пересекаются с TOC:/T:/
// ad:/dp:/ts:/tl:/tn:. Worst case "L3:" + 19 + 1 + 19 цифр = 42 байта < 64.
const (
	cbPrefixL1 = "L1:"
	cbPrefixL2 = "L2:"
	cbPrefixL3 = "L3:"
)

// Digest view refusal/error texts (plan §C — verbatim).
const (
	digestViewUnavailableText = "Просмотр дайджеста недоступен."
	digestViewBadIDText       = "❌ Некорректный запрос."
	digestViewNotFoundText    = "ℹ️ Дайджест не найден или устарел."
	digestViewErrorText       = "❌ Не удалось открыть."
)

// Drill-down texts (plan §D). «Не получилось открыть» — verbatim ack из плана
// (ошибка seam'а); «Drill-down недоступен.» — деградация при незаполненном
// seam (выбор D-1: ack + edit на месте, а не ack-only — под старым текстом
// не остаётся «живых» L2/L3-кнопок без объяснения).
const (
	drillDownErrorText       = "Не получилось открыть"
	drillDownUnavailableText = "Drill-down недоступен."
	// drillDownMoreLabel — статическая подпись L1-кнопки: 24 байта < 64
	// (лимит Bot API, урок A2 H1), усечение не нужно.
	drillDownMoreLabel = "Подробнее →"
)

// handleTopicOpen processes "T:<digestID>": the topic L1 view.
func (b *Bot) handleTopicOpen(ctx context.Context, q *models.CallbackQuery, arg string) {
	b.handleDigestView(ctx, q, arg, ViewKindTopic)
}

// handleTOC processes "TOC:<runID>": the run's table of contents.
func (b *Bot) handleTOC(ctx context.Context, q *models.CallbackQuery, arg string) {
	b.handleDigestView(ctx, q, arg, ViewKindTOC)
}

// handleDigestView is the shared T:/TOC: contour: ack → message guard →
// parse → viewer → edit in place.
func (b *Bot) handleDigestView(ctx context.Context, q *models.CallbackQuery, arg, kind string) {
	b.answerCallback(ctx, q.ID, "") // ack first, edit second (plan §5 notes)
	msg, ok := callbackMessage(q)
	if !ok {
		return // inaccessible message: ack only
	}

	id, err := strconv.ParseInt(arg, 10, 64)
	if err != nil {
		b.editCallbackMessage(ctx, msg, digestViewBadIDText, nil)
		return
	}
	if b.digestViewer == nil {
		b.editCallbackMessage(ctx, msg, digestViewUnavailableText, nil)
		return
	}
	view, ok, err := b.digestViewer(ctx, DigestViewRequest{Kind: kind, ID: id})
	if err != nil {
		b.log.Error("digest viewer failed", "kind", kind, "error", err)
		b.editCallbackMessage(ctx, msg, digestViewErrorText, nil)
		return
	}
	if !ok {
		b.editCallbackMessage(ctx, msg, digestViewNotFoundText, nil)
		return
	}
	buttons := view.Buttons
	if kind == ViewKindTopic {
		// Граница C/D (Итог C): RenderTopicL1 в digest заморожен — композицию
		// «Подробнее →» + «◀ К оглавлению» делает telegram-слой. T: и L1:
		// (alias выше в диспетчере) дают одинаковый view.
		buttons = withDrillDownButton(buttons, id)
	}
	b.editCallbackMessage(ctx, msg, view.Text, inlineKeyboard(buttons))
}

// withDrillDownButton adds the L1-view «Подробнее →» (L2:<digestID>) button
// NEXT TO the viewer's first-row button («◀ К оглавлению»), before it; with no
// viewer buttons it becomes the single row. view.Buttons is not mutated in
// place (rows are copied) — the viewer stub may reuse its slices.
func withDrillDownButton(buttons [][]InlineButton, digestID int64) [][]InlineButton {
	more := InlineButton{Text: drillDownMoreLabel, Data: cbPrefixL2 + strconv.FormatInt(digestID, 10)}
	if len(buttons) == 0 {
		return [][]InlineButton{{more}}
	}
	out := make([][]InlineButton, len(buttons))
	copy(out, buttons)
	out[0] = append([]InlineButton{more}, out[0]...)
	return out
}

// handleDrillDown processes "L2:…" / "L3:…" callbacks through the DrillDown
// seam (Stage 8 D): seam → ack (AckText из результата) → edit, если seam его
// вернул. Порядок ack-раньше-edit — как в ветках A2/B/C (plan §5 notes);
// ошибка seam'а → ack «Не получилось открыть» + лог, БЕЗ edit (plan §D).
// Аргументы data здесь не парсятся — q.Data уходит в seam как есть, включая
// мусор (сторона seam'а, D-2, отвечает «не найдено»/ошибкой).
func (b *Bot) handleDrillDown(ctx context.Context, q *models.CallbackQuery) {
	msg, ok := callbackMessage(q)
	if !ok {
		// Inaccessible message: только ack (plan §4.7), seam не вызывается —
		// редактировать нечего, результат всё равно не показать.
		b.answerCallback(ctx, q.ID, "")
		return
	}
	if b.drillDown == nil {
		b.answerCallback(ctx, q.ID, "")
		// Деградация без wiring'а: клавиатура ЗАМЕНЯется пустой, чтобы старые
		// L2/L3-кнопки не висели под текстом отказа (см. drillDownKeyboard).
		b.editCallbackMessage(ctx, msg, drillDownUnavailableText, drillDownKeyboard(nil))
		return
	}
	res, err := b.drillDown(ctx, q.Data)
	if err != nil {
		b.log.Error("drill-down failed", "data", q.Data, "error", err)
		b.answerCallback(ctx, q.ID, drillDownErrorText)
		return
	}
	if res == nil {
		// Defense: корректный seam возвращает non-nil; nil трактуем как «делать
		// нечего» (ack), а не паникуем.
		b.answerCallback(ctx, q.ID, "")
		return
	}
	b.answerCallback(ctx, q.ID, res.AckText) // ack first, edit second
	if res.Edit == nil {
		return // nil Edit → только ack (контракт CallbackResult)
	}
	b.editCallbackMessage(ctx, msg, res.Edit.Text, drillDownKeyboard(res.Edit.Buttons))
}

// drillDownKeyboard builds the markup that REPLACES the edited message's old
// keyboard. Unlike inlineKeyboard (nil for empty input → editCallbackMessage
// leaves the old markup in place, техдолг C LOW-3), empty Buttons yield a
// non-nil EMPTY markup: Bot API editMessageText accepts
// {"inline_keyboard": []} in reply_markup and clears the buttons — для
// drill-down-вью замена обязательна (фиксация поведения D-1, план §D).
func drillDownKeyboard(buttons [][]InlineButton) *models.InlineKeyboardMarkup {
	if kb := inlineKeyboard(buttons); kb != nil {
		return kb
	}
	return &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{}}
}
