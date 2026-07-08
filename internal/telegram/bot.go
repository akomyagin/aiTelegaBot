// Package telegram handles delivery and commands via the Bot API
// (github.com/go-telegram/bot, see docs/TECHNICAL_PLAN.md §4) and reading of
// managed channels/groups plus best-effort t.me/s scraping of public channels.
//
// The library is kept behind the Deliverer interface so a webhook or an
// alternative library can replace one implementation without touching the core.
//
// Этап 1: real long-polling bot with delivery and manual command routing.
// Source reading lands in Этап 5.
package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// Deliverer sends a rendered digest to the user's chat.
type Deliverer interface {
	Deliver(ctx context.Context, chatID, text string) error
}

// Bot wraps the go-telegram/bot client: long-polling, command routing, delivery.
type Bot struct {
	api         *bot.Bot // library client
	token       string   // never logged
	chatID      int64    // owner chat; only accept commands from this single user
	onDigest    func(ctx context.Context) error
	listSources func(ctx context.Context) (string, error)
	log         *slog.Logger
}

// Option configures the Bot.
type Option func(*Bot)

// WithDigestTrigger wires the /digest command to an on-demand digest run.
func WithDigestTrigger(fn func(ctx context.Context) error) Option {
	return func(b *Bot) { b.onDigest = fn }
}

// WithSourceLister wires the /sources command to a source listing.
func WithSourceLister(fn func(ctx context.Context) (string, error)) Option {
	return func(b *Bot) { b.listSources = fn }
}

// NewBot builds the Telegram bot over the go-telegram/bot library. The chatID
// (owner) is parsed from its string form; the bot token is never logged.
func NewBot(token, chatID string, opts ...Option) (*Bot, error) {
	id, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("telegram: invalid chat id: %w", err)
	}

	b := &Bot{
		token:  token,
		chatID: id,
		log:    slog.Default(),
	}
	for _, opt := range opts {
		opt(b)
	}

	api, err := bot.New(token, bot.WithDefaultHandler(b.handleUpdate))
	if err != nil {
		return nil, fmt.Errorf("telegram: create bot: %w", err)
	}
	b.api = api

	return b, nil
}

// Run starts long-polling and blocks until ctx is cancelled (graceful shutdown).
func (b *Bot) Run(ctx context.Context) error {
	b.api.Start(ctx)
	return ctx.Err()
}

// Deliver sends text to the given chat. An empty chatID targets the owner chat
// configured on the bot. Errors are wrapped without exposing the token.
func (b *Bot) Deliver(ctx context.Context, chatID, text string) error {
	target := b.chatID
	if chatID != "" {
		id, err := strconv.ParseInt(chatID, 10, 64)
		if err != nil {
			return fmt.Errorf("telegram deliver: invalid chat id: %w", err)
		}
		target = id
	}

	if _, err := b.api.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:    target,
		Text:      text,
		ParseMode: models.ParseModeHTML,
	}); err != nil {
		return fmt.Errorf("telegram deliver: %w", err)
	}
	return nil
}
