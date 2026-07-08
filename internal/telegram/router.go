package telegram

import (
	"context"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// Command reply texts.
const (
	startText = "Привет! Я AI-дайджест бот. Собираю контент из RSS, Hacker News " +
		"и Telegram-каналов и присылаю сжатую сводку по расписанию.\n\n" +
		"/digest — получить дайджест сейчас\n" +
		"/sources — список источников\n" +
		"/help — справка"

	helpText = "Команды:\n" +
		"/start — приветствие\n" +
		"/digest — получить дайджест сейчас\n" +
		"/sources — список источников\n" +
		"/help — эта справка"

	digestStartedText = "⏳ Собираю дайджест…"
	digestErrorText   = "❌ Ошибка при сборке дайджеста"
	digestUnavailable = "Дайджест пока недоступен"
	sourcesUnavailable = "Источники появятся в следующих версиях"
	unknownCommandText = "Неизвестная команда. /help — список команд"
)

// handleUpdate is the single default handler: it filters non-message and
// unauthorized updates, then routes recognized commands.
func (b *Bot) handleUpdate(ctx context.Context, api *bot.Bot, u *models.Update) {
	_ = api // delivery goes through b.Deliver

	if u.Message == nil {
		return // channel posts / callbacks are out of scope for Этап 1
	}

	if !authorized(u.Message.Chat.ID, b.chatID) {
		b.log.Warn("ignoring update from unauthorized chat", "chat_id", u.Message.Chat.ID)
		return
	}

	cmd, ok := parseCommand(u.Message.Text)
	if !ok {
		b.reply(ctx, unknownCommandText)
		return
	}

	switch cmd {
	case "start":
		b.reply(ctx, startText)
	case "help":
		b.reply(ctx, helpText)
	case "digest":
		b.handleDigest(ctx)
	case "sources":
		b.handleSources(ctx)
	default:
		b.reply(ctx, unknownCommandText)
	}
}

// handleDigest acknowledges immediately and runs the trigger in the background;
// error details (which may reference secrets) are never surfaced to the user.
func (b *Bot) handleDigest(ctx context.Context) {
	b.reply(ctx, digestStartedText)
	if b.onDigest == nil {
		b.reply(ctx, digestUnavailable)
		return
	}
	go func() {
		if err := b.onDigest(ctx); err != nil {
			b.log.Error("on-demand digest failed", "error", err)
			b.reply(ctx, digestErrorText)
		}
	}()
}

func (b *Bot) handleSources(ctx context.Context) {
	if b.listSources == nil {
		b.reply(ctx, sourcesUnavailable)
		return
	}
	text, err := b.listSources(ctx)
	if err != nil {
		b.log.Error("listing sources failed", "error", err)
		b.reply(ctx, sourcesUnavailable)
		return
	}
	b.reply(ctx, text)
}

// reply delivers to the owner chat, logging (not surfacing) any send error.
func (b *Bot) reply(ctx context.Context, text string) {
	if err := b.Deliver(ctx, "", text); err != nil {
		b.log.Error("sending reply failed", "error", err)
	}
}

// authorized reports whether an incoming chat matches the configured owner.
func authorized(incoming, owner int64) bool {
	return incoming == owner
}

// parseCommand extracts a bare command name from message text. It returns
// ("", false) when the text is not a command. The leading slash, any trailing
// arguments, and an @botname suffix are stripped; the result is lower-cased.
func parseCommand(text string) (cmd string, ok bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return "", false
	}

	word := strings.Fields(text)[0]      // safe: text starts with "/", non-empty
	word = strings.TrimPrefix(word, "/") // drop leading slash
	word = strings.SplitN(word, "@", 2)[0]
	if word == "" {
		return "", false
	}
	return strings.ToLower(word), true
}
