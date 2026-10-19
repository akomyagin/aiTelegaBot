package telegram

import (
	"context"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// ForwardOrigin identifies the chat/channel a forwarded message came from.
// Extraction happens in this package (the only place that knows the Bot API
// types); the registration decision lives in the app-level ForwardRegistrar
// closure (plan Stage 8 A1).
type ForwardOrigin struct {
	BotAPIChatID int64  // id of the source chat (channel/supergroup), -100X form
	Username     string // "" for private chats
	Title        string // display name
	BotIsAdmin   bool   // whether THIS bot administers the chat (checked at extraction)
}

// ForwardRegistrar registers a source from a forward and returns the reply
// text. Replies are delivered with ParseMode HTML, so the registrar must
// escape dynamic parts (EscapeHTML).
type ForwardRegistrar func(ctx context.Context, origin ForwardOrigin) (reply string)

// forwardUnsupportedText is the refusal for forwards of personal
// correspondence (user / hidden_user origins): only channels and chats can
// become sources.
const forwardUnsupportedText = "Могу добавлять только каналы и чаты, не личные переписки."

// extractForwardOrigin converts the Bot API MessageOrigin wrapper into a
// ForwardOrigin. ok=false for origins that cannot become a source
// (MessageOriginUser, MessageOriginHiddenUser, or a malformed payload).
func extractForwardOrigin(o *models.MessageOrigin) (ForwardOrigin, bool) {
	if o == nil {
		return ForwardOrigin{}, false
	}
	switch o.Type {
	case models.MessageOriginTypeChannel:
		if c := o.MessageOriginChannel; c != nil {
			return ForwardOrigin{
				BotAPIChatID: c.Chat.ID,
				Username:     c.Chat.Username,
				Title:        chatDisplayName(c.Chat),
			}, true
		}
	case models.MessageOriginTypeChat:
		if c := o.MessageOriginChat; c != nil {
			return ForwardOrigin{
				BotAPIChatID: c.SenderChat.ID,
				Username:     c.SenderChat.Username,
				Title:        chatDisplayName(c.SenderChat),
			}, true
		}
	}
	return ForwardOrigin{}, false
}

// chatDisplayName picks the best display name for a chat.
func chatDisplayName(c models.Chat) string {
	switch {
	case c.Title != "":
		return c.Title
	case c.Username != "":
		return "@" + c.Username
	default:
		return "чат"
	}
}

// handleForward processes a forwarded message from the owner: it extracts the
// origin, checks the bot's admin status, and delegates registration to the
// wired ForwardRegistrar. Called only from inside the authorized() contour of
// handleUpdate (plan §4.7: no new trust boundaries).
func (b *Bot) handleForward(ctx context.Context, o *models.MessageOrigin) {
	origin, ok := extractForwardOrigin(o)
	if !ok {
		b.reply(ctx, forwardUnsupportedText)
		return
	}
	if b.forwardRegistrar == nil {
		b.reply(ctx, "Добавление источников недоступно.")
		return
	}
	origin.BotIsAdmin = b.checkForwardAdmin(ctx, origin)
	b.reply(ctx, b.forwardRegistrar(ctx, origin))
}

// checkForwardAdmin reports whether the bot administers the forwarded-from
// chat: by @username for public chats, by numeric id for private ones (plan
// A1: bot-admin private chats are read through the Bot API directly, MTProto
// is not needed). Any error means "not admin" and the registrar falls back to
// tg_public / tg_mtproto. No-op when the library client is not initialized
// (unit tests).
func (b *Bot) checkForwardAdmin(ctx context.Context, origin ForwardOrigin) bool {
	if b.api == nil {
		return false
	}
	var chatID any
	if origin.Username != "" {
		chatID = "@" + origin.Username
	} else {
		chatID = origin.BotAPIChatID
	}
	member, err := b.api.GetChatMember(ctx, &bot.GetChatMemberParams{
		ChatID: chatID,
		UserID: b.api.ID(),
	})
	if err != nil {
		b.log.Info("forward: getChatMember failed, treating as not admin",
			"chat_id", origin.BotAPIChatID, "err", err)
		return false
	}
	return member.Type == models.ChatMemberTypeAdministrator ||
		member.Type == models.ChatMemberTypeOwner
}
