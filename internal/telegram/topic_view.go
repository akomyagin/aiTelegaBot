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

// Callback data prefixes (plan §4.4/§4.6, Этап 9D): все префиксы попарно не
// пересекаются (второй байт различается) и не конфликтуют с ad:/dp:/ts:/tl:/
// tn:. Worst case «TOC:» + 19 цифр = 23 байта, «S:» + 19 + 1 + 19 = 41 байт —
// оба < 64 (property-тест тест-кейса 5 плана). Диспетчер проверяет TOC: раньше
// T: (читаемость); «PDEL» не начинается с «P:», порядок P:→PDEL не критичен.
const (
	cbPrefixTOC       = "TOC:"
	cbPrefixTopicOpen = "T:"
	// Этап 9D (§4.6): «S:<digestID>:<sourceID>» — топик (sourceID=0 → все
	// топики одним списком с подзаголовками), «M:<digestItemID>» — сообщение,
	// «P:<digestItemID>» — фото (9D: ack-заглушка «Медиа недоступно»,
	// sendPhoto — 9E), «PDEL» — закрыть фото (9D: ack-only, deleteMessage — 9E).
	cbPrefixSource  = "S:"
	cbPrefixMessage = "M:"
	cbPrefixPhoto   = "P:"
	cbPhotoDelete   = "PDEL"
)

// Legacy drill-down prefixes Этапа 8 D — в 9D остаются ALIAS'ами кнопок СТАРЫХ
// доставленных сообщений (§4.6): «L1:<did>» → как «T:<did>» (viewer-путь,
// диспетчер dialogs.go), «L2:<did>» → как «T:<did>» (ближайший новый аналог,
// диспетчер digest.NewDrillDown), «L3:<did>:<sid>» → «S:<did>:<sid>» (включая
// sid=0). Worst case «L3:» + 19 + 1 + 19 цифр = 42 байта < 64.
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
	// photoUnavailableText — 9D-заглушка «P:» (план 9D: фото отправляется в
	// 9E): только ack, БЕЗ edit.
	photoUnavailableText = "Медиа недоступно"
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
	// Этап 9D: клавиатуру T:-view полностью собирает viewer (RenderTopicView:
	// кнопки-имена топиков S:<did>:<sid> + «◀ К оглавлению» TOC:<runID>);
	// telegram ничего не докомпановывает — «Подробнее →» из Этапа 8 D больше
	// нет (L1-view сам перечисляет топики). T: и L1: (alias в диспетчере)
	// дают одинаковый view.
	//
	// Ревью 9D L5: T-view идёт через drillDownKeyboard — при пустых Buttons
	// inlineKeyboard дал бы nil, и editCallbackMessage сохранил бы СТАРЫЙ
	// markup (legacy run_id=0 без items: старые кнопки под новым текстом);
	// non-nil пустой markup заставляет Bot API убрать старые кнопки.
	if kind == ViewKindTopic {
		b.editCallbackMessage(ctx, msg, view.Text, drillDownKeyboard(view.Buttons))
		return
	}
	b.editCallbackMessage(ctx, msg, view.Text, inlineKeyboard(view.Buttons))
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
