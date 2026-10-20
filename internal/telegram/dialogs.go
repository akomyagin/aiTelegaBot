package telegram

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/akomyagin/aiTelegaBot/internal/mtproto"
)

// Stage 8 A2: the /dialogs command lists the owner's MTProto dialogs and lets
// them register a channel/supergroup as a source with one button press.
// Registration reuses the A1 forward path (ForwardRegistrar): the same
// admin-check → tg_botapi / tg_public / tg_mtproto decision and validation.

const (
	dialogsPerPage    = 8
	dialogsFetchLimit = 100
	// dialogsRateLimit bounds real GetDialogs calls (plan §4.6 FloodWait
	// discipline): a repeated /dialogs within the window reuses the last
	// successful in-memory snapshot.
	dialogsRateLimit = 30 * time.Second
	// dialogsFetchTimeout bounds the lock hold time in cachedDialogs (review
	// M1): deliberately larger than a normal GetDialogs RPC and smaller than
	// a FloodWait tail, so a sleeping lister cannot block all dialogs
	// callbacks indefinitely.
	dialogsFetchTimeout = 30 * time.Second

	dialogsUnavailableText = "/dialogs требует настроенный MTProto-аккаунт (MTPROTO_APP_ID и MTPROTO_APP_HASH)."
	dialogsErrorText       = "❌ Не удалось получить список диалогов."
	dialogsEmptyText       = "Диалогов не найдено."
	dialogsStaleText       = "Список диалогов устарел — отправьте /dialogs заново."
	dialogAddForbiddenText = "Можно добавлять только каналы и супергруппы: личные переписки и обычные группы вне дайджеста."
	dialogBadIDText        = "❌ Некорректный id диалога."
	dialogBadPageText      = "❌ Некорректный номер страницы."
	addSourceUnavailable   = "Добавление источников недоступно."
	cbNotOwnerText         = "Не ваш бот"

	// Callback data prefixes (plan §4.4): parsed with strings.CutPrefix +
	// strings.Cut-style splits, never regex. Worst case "ad:" + 19 digits =
	// 22 bytes < 64.
	cbPrefixDialogAdd  = "ad:"
	cbPrefixDialogPage = "dp:"
)

// DialogLister fetches the owner's MTProto dialogs (Stage 8 A2). nil on Bot
// means MTProto is not configured and /dialogs degrades to a first-class
// refusal (user decision 2026-10-20: VPS may run without MTProto).
type DialogLister func(ctx context.Context, limit int) ([]mtproto.PeerInfo, error)

// handleDialogs renders the first page of the dialog list. Called only from
// inside the authorized() contour of handleUpdate.
func (b *Bot) handleDialogs(ctx context.Context) {
	if b.dialogLister == nil {
		b.reply(ctx, dialogsUnavailableText)
		return
	}
	peers, err := b.cachedDialogs(ctx)
	if err != nil {
		// GetDialogs errors carry RPC codes only — safe to log, not to show.
		b.log.Error("fetching dialogs failed", "error", err)
		b.reply(ctx, dialogsErrorText)
		return
	}
	text, kb := renderDialogsPage(peers, 0)
	b.sendPage(ctx, text, kb)
}

// cachedDialogs returns the in-memory dialog snapshot, refreshing it via the
// DialogLister only when the last successful fetch is older than
// dialogsRateLimit. The mutex is held across the fetch so concurrent calls
// cannot stampede GetDialogs (single-user service, contention is nil).
func (b *Bot) cachedDialogs(ctx context.Context) ([]mtproto.PeerInfo, error) {
	b.dialogsMu.Lock()
	defer b.dialogsMu.Unlock()

	now := time.Now()
	if b.nowFn != nil {
		now = b.nowFn()
	}
	if b.dialogsCached && now.Sub(b.dialogsFetchedAt) < dialogsRateLimit {
		return b.dialogsSnapshot, nil
	}
	fetchCtx, cancel := context.WithTimeout(ctx, dialogsFetchTimeout)
	defer cancel()
	peers, err := b.dialogLister(fetchCtx, dialogsFetchLimit)
	if err != nil {
		return nil, err
	}
	b.dialogsSnapshot = peers
	b.dialogsFetchedAt = now
	b.dialogsCached = true
	return peers, nil
}

// renderDialogsPage renders one page (dialogsPerPage rows) of the dialog list
// plus its inline keyboard: an «Добавить» button per channel/supergroup row
// (callback data "ad:<botAPIID>") and «◀ / ▶» navigation ("dp:<offset>").
// user/chat dialogs are listed WITHOUT an add button — personal chats are out
// of the digest scope (Stage 8 boundary). All dynamic text is HTML-escaped:
// the message goes out with ParseMode HTML (plan §4.8). Button labels are
// plain text (no parse mode) and are NOT escaped.
func renderDialogsPage(peers []mtproto.PeerInfo, offset int) (string, *models.InlineKeyboardMarkup) {
	if len(peers) == 0 {
		return dialogsEmptyText, nil
	}
	if offset < 0 {
		offset = 0
	}
	if offset >= len(peers) {
		offset = (len(peers) - 1) / dialogsPerPage * dialogsPerPage
	}
	// Align a crafted non-multiple offset (e.g. dp:3) to its page start, so
	// navigation buttons always land on aligned offsets (review L3). The
	// clamp branch above already yields an aligned offset.
	offset = offset / dialogsPerPage * dialogsPerPage
	page := peers[offset:]
	if len(page) > dialogsPerPage {
		page = page[:dialogsPerPage]
	}

	total := (len(peers) + dialogsPerPage - 1) / dialogsPerPage
	current := offset/dialogsPerPage + 1

	var text strings.Builder
	fmt.Fprintf(&text, "Мои диалоги (страница %d из %d):\n", current, total)
	var rows [][]models.InlineKeyboardButton
	for _, p := range page {
		fmt.Fprintf(&text, "• %s\n", dialogLine(p))
		if (p.Kind == "channel" || p.Kind == "supergroup") && p.BotAPIID != 0 {
			rows = append(rows, []models.InlineKeyboardButton{{
				Text:         "Добавить: " + truncateButtonLabel(dialogLabel(p)),
				CallbackData: cbPrefixDialogAdd + strconv.FormatInt(p.BotAPIID, 10),
			}})
		}
	}

	var nav []models.InlineKeyboardButton
	if offset > 0 {
		nav = append(nav, models.InlineKeyboardButton{
			Text:         "◀",
			CallbackData: cbPrefixDialogPage + strconv.Itoa(offset-dialogsPerPage),
		})
	}
	if offset+dialogsPerPage < len(peers) {
		nav = append(nav, models.InlineKeyboardButton{
			Text:         "▶",
			CallbackData: cbPrefixDialogPage + strconv.Itoa(offset+dialogsPerPage),
		})
	}
	if len(nav) > 0 {
		rows = append(rows, nav)
	}
	if len(rows) == 0 {
		return text.String(), nil
	}
	return text.String(), &models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// dialogLine renders one escaped list row: `<title> [@username] (<kind>)`.
func dialogLine(p mtproto.PeerInfo) string {
	title := p.Title
	if title == "" {
		title = dialogLabel(p)
	}
	line := EscapeHTML(title)
	if p.Title != "" && p.Username != "" {
		line += " [@" + EscapeHTML(p.Username) + "]"
	}
	return line + " (" + EscapeHTML(p.Kind) + ")"
}

// dialogLabel is the plain-text display name fallback (button labels, rows
// without a title).
func dialogLabel(p mtproto.PeerInfo) string {
	switch {
	case p.Title != "":
		return p.Title
	case p.Username != "":
		return "@" + p.Username
	default:
		return "диалог " + strconv.FormatInt(p.MTProtoID, 10)
	}
}

// buttonLabelBudget is the byte budget for the dynamic part of an inline
// button label: Bot API rejects buttons whose text exceeds 64 BYTES
// (BUTTON_TEXT_INVALID), so the budget is 64 − len("Добавить: ") (18 bytes) −
// len("…") (3 bytes) = 43 bytes (review H1).
const buttonLabelBudget = 64 - len("Добавить: ") - len("…")

// truncateButtonLabel shortens s to buttonLabelBudget bytes, cutting on a
// rune boundary (utf8.DecodeLastRuneInString) and appending "…".
func truncateButtonLabel(s string) string {
	return truncateButtonLabelTo(s, buttonLabelBudget)
}

// truncateButtonLabelTo is truncateButtonLabel with a caller-provided byte
// budget (Stage 8 B: the topic picker uses the full 64−len("…") budget since
// its labels carry no «Добавить: » prefix). Cuts on a rune boundary.
func truncateButtonLabelTo(s string, budget int) string {
	if len(s) <= budget {
		return s
	}
	for len(s) > budget {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s + "…"
}

// handleCallbackQuery routes a CallbackQuery (Stage 8 A2 skeleton; Stage 8 D
// extends it into the full drill-down/topic dispatcher). Every callback is
// answered (plan §4.7). Prefix parsing is strings.CutPrefix, not regex.
func (b *Bot) handleCallbackQuery(ctx context.Context, q *models.CallbackQuery) {
	// Owner check: in the single-user private chat the owner's user id equals
	// the chat id (the existing TELEGRAM_CHAT_ID assumption), so From.ID is
	// compared against b.chatID (plan §4.7). A stranger's callback gets the
	// "not your bot" ack and NO action.
	if q.From.ID != b.chatID {
		b.log.Warn("ignoring callback from unauthorized user", "user_id", q.From.ID)
		b.answerCallback(ctx, q.ID, cbNotOwnerText)
		return
	}

	if rest, ok := strings.CutPrefix(q.Data, cbPrefixDialogAdd); ok {
		b.handleDialogAdd(ctx, q, rest)
		return
	}
	if rest, ok := strings.CutPrefix(q.Data, cbPrefixDialogPage); ok {
		b.handleDialogPage(ctx, q, rest)
		return
	}
	// Stage 8 B: topic prefixes (план §4.4/§4.11) — тот же диспетчер, owner-чек
	// и ack-контур выше; обработчики в topics_admin.go. Второй диспетчер НЕ
	// заводится. Порядок CutPrefix безопасен: ts:/tl:/tn: не пересекаются с
	// ad:/dp: (и будущими L1/L2/L3).
	if rest, ok := strings.CutPrefix(q.Data, cbPrefixTopicSet); ok {
		b.handleTopicSetCallback(ctx, q, rest)
		return
	}
	if rest, ok := strings.CutPrefix(q.Data, cbPrefixTopicList); ok {
		b.handleTopicListCallback(ctx, q, rest)
		return
	}
	if rest, ok := strings.CutPrefix(q.Data, cbPrefixTopicNone); ok {
		b.handleTopicNoneCallback(ctx, q, rest)
		return
	}
	if b.callbackHandler != nil {
		// Stage 8 D owns ack + dispatch for the remaining prefixes.
		b.callbackHandler(ctx, q)
		return
	}
	// Unknown prefix: still ack so the client stops showing the spinner.
	b.answerCallback(ctx, q.ID, "")
}

// callbackMessage extracts the message a callback was pressed on. In
// go-telegram/bot v1.22 MaybeInaccessibleMessage carries a concrete *Message
// which is nil for inaccessible (too old) messages — the equivalent of the
// plan's "type-assert to *models.Message": nil means "ack only, no edit".
func callbackMessage(q *models.CallbackQuery) (*models.Message, bool) {
	return q.Message.Message, q.Message.Message != nil
}

// handleDialogAdd processes "ad:<botAPIID>": find the peer in the snapshot,
// refuse non-channel kinds, then register through the A1 ForwardRegistrar
// (admin check here — the app has no bot client; kind/ref choice, validation
// and persistence in the closure) and edit the message with the reply.
func (b *Bot) handleDialogAdd(ctx context.Context, q *models.CallbackQuery, arg string) {
	b.answerCallback(ctx, q.ID, "") // ack first, edit second (plan §5 notes)
	msg, ok := callbackMessage(q)
	if !ok {
		return // inaccessible message: ack only
	}

	id, err := strconv.ParseInt(arg, 10, 64)
	if err != nil {
		b.editCallbackMessage(ctx, msg, dialogBadIDText, nil)
		return
	}
	if b.dialogLister == nil {
		b.editCallbackMessage(ctx, msg, dialogsUnavailableText, nil)
		return
	}
	peers, err := b.cachedDialogs(ctx)
	if err != nil {
		b.log.Error("fetching dialogs failed", "error", err)
		b.editCallbackMessage(ctx, msg, dialogsErrorText, nil)
		return
	}
	peer, found := findPeerByBotAPIID(peers, id)
	if !found {
		b.editCallbackMessage(ctx, msg, dialogsStaleText, nil)
		return
	}
	// Stage boundary: user (personal chats) and chat (basic groups) are not
	// digest material. No button is rendered for them; a crafted callback is
	// refused here.
	if peer.Kind != "channel" && peer.Kind != "supergroup" {
		b.editCallbackMessage(ctx, msg, dialogAddForbiddenText, nil)
		return
	}
	if b.forwardRegistrar == nil && b.forwardRegistrarV2 == nil {
		b.editCallbackMessage(ctx, msg, addSourceUnavailable, nil)
		return
	}

	origin := ForwardOrigin{
		BotAPIChatID: peer.BotAPIID,
		Username:     peer.Username,
		Title:        peer.Title,
	}
	origin.BotIsAdmin = b.checkForwardAdmin(ctx, origin)
	if b.forwardRegistrarV2 != nil {
		// Stage 8 B: расширенный регистратор возвращает sourceID/kind/ref —
		// suggested-поток (§4.11) добавляет ts/tl/tn-кнопки без парсинга текста.
		res := b.forwardRegistrarV2(ctx, origin)
		text, kb := b.withTopicSuggestion(ctx, res.Reply, res.SourceID, res.Kind, res.Ref)
		b.editCallbackMessage(ctx, msg, text, kb)
		return
	}
	b.editCallbackMessage(ctx, msg, b.forwardRegistrar(ctx, origin), nil)
}

// handleDialogPage processes "dp:<offset>": re-render the list page in place.
func (b *Bot) handleDialogPage(ctx context.Context, q *models.CallbackQuery, arg string) {
	b.answerCallback(ctx, q.ID, "")
	msg, ok := callbackMessage(q)
	if !ok {
		return
	}
	offset, err := strconv.Atoi(arg)
	if err != nil || offset < 0 {
		b.editCallbackMessage(ctx, msg, dialogBadPageText, nil)
		return
	}
	if b.dialogLister == nil {
		b.editCallbackMessage(ctx, msg, dialogsUnavailableText, nil)
		return
	}
	peers, err := b.cachedDialogs(ctx)
	if err != nil {
		b.log.Error("fetching dialogs failed", "error", err)
		b.editCallbackMessage(ctx, msg, dialogsErrorText, nil)
		return
	}
	text, kb := renderDialogsPage(peers, offset)
	b.editCallbackMessage(ctx, msg, text, kb)
}

// findPeerByBotAPIID returns the first snapshot peer with the given Bot API
// id. user/chat peers share BotAPIID==0 — they are rejected by the kind gate
// in handleDialogAdd before registration, so the first match is enough.
func findPeerByBotAPIID(peers []mtproto.PeerInfo, id int64) (mtproto.PeerInfo, bool) {
	for _, p := range peers {
		if p.BotAPIID == id {
			return p, true
		}
	}
	return mtproto.PeerInfo{}, false
}

// answerCallback acknowledges a callback query (mandatory for every callback,
// plan §4.7). ackFn is the test seam; nil => the library client; nil api
// (unit tests without a client) => no-op.
func (b *Bot) answerCallback(ctx context.Context, callbackQueryID, text string) {
	if b.ackFn != nil {
		if err := b.ackFn(ctx, callbackQueryID, text); err != nil {
			b.log.Error("answering callback failed", "error", err)
		}
		return
	}
	if b.api == nil {
		return
	}
	if _, err := b.api.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
		CallbackQueryID: callbackQueryID,
		Text:            text,
	}); err != nil {
		b.log.Error("answering callback failed", "error", err)
	}
}

// sendPage sends the dialogs list page with its inline keyboard to the owner.
// sendFn is the test seam; nil => the library client.
func (b *Bot) sendPage(ctx context.Context, text string, kb *models.InlineKeyboardMarkup) {
	if b.sendFn != nil {
		if err := b.sendFn(ctx, b.chatID, text, kb); err != nil {
			b.log.Error("sending dialogs page failed", "error", err)
		}
		return
	}
	if b.api == nil {
		return
	}
	params := &bot.SendMessageParams{
		ChatID:    b.chatID,
		Text:      text,
		ParseMode: models.ParseModeHTML,
	}
	if kb != nil {
		params.ReplyMarkup = kb
	}
	if _, err := b.api.SendMessage(ctx, params); err != nil {
		b.log.Error("sending dialogs page failed", "error", err)
	}
}

// editCallbackMessage edits the message a callback was pressed on (dialogs
// navigation and registration results live in place, no message spam).
// editFn is the test seam; nil => the library client.
func (b *Bot) editCallbackMessage(ctx context.Context, msg *models.Message, text string, kb *models.InlineKeyboardMarkup) {
	if b.editFn != nil {
		if err := b.editFn(ctx, msg.Chat.ID, msg.ID, text, kb); err != nil {
			b.log.Error("editing callback message failed", "error", err)
		}
		return
	}
	if b.api == nil {
		return
	}
	params := &bot.EditMessageTextParams{
		ChatID:    msg.Chat.ID,
		MessageID: msg.ID,
		Text:      text,
		ParseMode: models.ParseModeHTML,
	}
	if kb != nil {
		params.ReplyMarkup = kb
	}
	if _, err := b.api.EditMessageText(ctx, params); err != nil {
		b.log.Error("editing callback message failed", "error", err)
	}
}
