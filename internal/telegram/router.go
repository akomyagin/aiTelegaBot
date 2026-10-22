package telegram

import (
	"context"
	"strconv"
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
		"/addsource [url|@channel|t.me-ссылка] — добавить источник\n" +
		"/removesource [id] — отключить источник\n" +
		"/topics — темы и их источники\n" +
		"/newtopic [имя] — новая тема\n" +
		"/settopic [id источника] [id темы] — привязать\n" +
		"/dialogs — список моих Telegram-диалогов, добавление кнопкой\n" +
		"/help — справка\n\n" +
		"Дайджест: выпуск → тема → канал → сообщение\n\n" +
		"Перешлите любое сообщение из канала или чата — добавлю его как источник.\n" +
		"/addsource понимает t.me-ссылки, включая приватные t.me/c/…"

	helpText = "Команды:\n" +
		"/start — приветствие\n" +
		"/digest — получить дайджест сейчас\n" +
		"/sources — список источников\n" +
		"/addsource [url|@channel|t.me-ссылка] — добавить источник\n" +
		"/removesource [id] — отключить источник\n" +
		"/topics — темы и их источники\n" +
		"/newtopic [имя] — новая тема\n" +
		"/settopic [id источника] [id темы] — привязать\n" +
		"/dialogs — список моих Telegram-диалогов, добавление кнопкой\n" +
		"/help — эта справка\n\n" +
		"Дайджест: выпуск → тема → канал → сообщение\n\n" +
		"Перешлите любое сообщение из канала или чата — добавлю его как источник.\n" +
		"/addsource понимает t.me-ссылки, включая приватные t.me/c/…"

	digestStartedText  = "⏳ Собираю дайджест…"
	digestErrorText    = "❌ Ошибка при сборке дайджеста"
	digestUnavailable  = "Дайджест пока недоступен"
	sourcesUnavailable = "Источники появятся в следующих версиях"
	unknownCommandText = "Неизвестная команда. /help — список команд"
)

// handleUpdate is the single default handler: it filters non-message and
// unauthorized updates, then routes recognized commands.
func (b *Bot) handleUpdate(ctx context.Context, api *bot.Bot, u *models.Update) {
	_ = api // delivery goes through b.Deliver

	// Channel posts from managed channels (bot is admin). Checked before the
	// u.Message guard, otherwise channel posts would be dropped. Buffer key:
	// the username, or the string form of the chat id for private chats
	// (plan §4.5) — the same key ManagedSource is constructed with.
	if u.ChannelPost != nil && b.channelBuf != nil {
		post := u.ChannelPost
		username := post.Chat.Username
		key := username
		if key == "" {
			key = strconv.FormatInt(post.Chat.ID, 10)
		}
		item := channelPostToItem(key, username, post.ID, post.Text, int64(post.Date))
		// Медиа (plan §4.5, Stage 9E): из ChannelPost.Photo берём largest-size
		// file_id. Альбом (MediaGroupID ≠ "") — фото несёт только ПЕРВЫЙ пост
		// группы: последующие посты той же группы идут с пустыми медиа-полями,
		// детектор «первый» — seen-множество ChannelBuffer (см.
		// MarkMediaGroupFirst: апдейты батча обрабатываются последовательно).
		if fileID := largestPhotoFileID(post.Photo); fileID != "" &&
			(post.MediaGroupID == "" || b.channelBuf.MarkMediaGroupFirst(post.MediaGroupID)) {
			item.MediaKind = "photo"
			item.MediaFileID = fileID
		}
		b.channelBuf.Push(key, item)
		return
	}

	// Callback queries (Stage 8 A2): owner check, ack and dispatch happen in
	// handleCallbackQuery. Checked before the u.Message guard — a callback
	// update carries no Message.
	if u.CallbackQuery != nil {
		b.handleCallbackQuery(ctx, u.CallbackQuery)
		return
	}

	if u.Message == nil {
		return // other updates are out of scope
	}

	if !authorized(u.Message.Chat.ID, b.chatID) {
		b.log.Warn("ignoring update from unauthorized chat", "chat_id", u.Message.Chat.ID)
		return
	}

	// Forwarded message from the owner: candidate source registration
	// (Stage 8 A1). Checked before command parsing so a forwarded
	// command-looking text is never treated as a command.
	if u.Message.ForwardOrigin != nil {
		b.handleForward(ctx, u.Message.ForwardOrigin)
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
	case "dialogs":
		b.handleDialogs(ctx)
	// Stage 8 B (темы). Дайджест по темам в справке НЕ обещаем — заработает в C.
	case "topics":
		b.handleTopics(ctx)
	case "newtopic":
		b.handleNewTopic(ctx, parseCommandArg(u.Message.Text))
	case "settopic":
		b.handleSetTopic(ctx, parseCommandArg(u.Message.Text))
	case "addsource":
		b.handleAddSource(ctx, parseCommandArg(u.Message.Text))
	case "removesource":
		b.handleRemoveSource(ctx, parseCommandArg(u.Message.Text))
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

// parseCommandArg returns the trimmed argument portion after the command word.
// For "/addsource https://x" it returns "https://x". Empty when no argument.
func parseCommandArg(text string) string {
	text = strings.TrimSpace(text)
	fields := strings.Fields(text)
	if len(fields) < 2 {
		return ""
	}
	return strings.TrimSpace(strings.Join(fields[1:], " "))
}
