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

// Digest view refusal/error texts (plan §C — verbatim).
const (
	digestViewUnavailableText = "Просмотр дайджеста недоступен."
	digestViewBadIDText       = "❌ Некорректный запрос."
	digestViewNotFoundText    = "ℹ️ Дайджест не найден или устарел."
	digestViewErrorText       = "❌ Не удалось открыть."
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
	b.editCallbackMessage(ctx, msg, view.Text, inlineKeyboard(view.Buttons))
}
