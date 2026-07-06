// Package telegram handles delivery and commands via the Bot API
// (github.com/go-telegram/bot, see docs/TECHNICAL_PLAN.md §4) and reading of
// managed channels/groups plus best-effort t.me/s scraping of public channels.
//
// The library is kept behind the Deliverer / SourceReader interfaces so a
// webhook or alternative library can replace one implementation.
//
// Stage 0: interfaces + stubs only — real bot lands in Этап 1 (delivery/commands)
// and Этап 5 (source reading).
package telegram

import "context"

// Deliverer sends a rendered digest to the user's chat.
type Deliverer interface {
	Deliver(ctx context.Context, chatID, text string) error
}

// Bot wraps the go-telegram/bot client: long-polling, command routing,
// delivery. Stage 0: stub.
type Bot struct {
	token  string // never logged
	chatID string // only accept commands from this single user
}

// NewBot builds the Telegram bot. Stage 0: stub.
func NewBot(token, chatID string) *Bot {
	return &Bot{token: token, chatID: chatID}
}

// Run starts long-polling and blocks until ctx is cancelled (graceful
// shutdown). Stage 0: stub.
func (b *Bot) Run(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

// Deliver sends text to the given chat. Stage 0: stub.
func (b *Bot) Deliver(ctx context.Context, chatID, text string) error {
	_ = ctx
	_ = chatID
	_ = text
	return nil
}
