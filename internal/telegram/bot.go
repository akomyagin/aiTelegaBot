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
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/akomyagin/aiTelegaBot/internal/mtproto"
)

// Deliverer sends a rendered digest to the user's chat.
type Deliverer interface {
	Deliver(ctx context.Context, chatID, text string) error
}

// Bot wraps the go-telegram/bot client: long-polling, command routing, delivery.
type Bot struct {
	api          *bot.Bot // library client
	token        string   // never logged
	chatID       int64    // owner chat; only accept commands from this single user
	onDigest     func(ctx context.Context) error
	listSources  func(ctx context.Context) (string, error)
	addSource    func(ctx context.Context, kind, ref string) (id int64, freshCount int, err error)
	removeSource func(ctx context.Context, id int64) (found bool, err error)
	// forwardRegistrar registers a source from a forwarded message (Stage 8
	// A1); nil => forwards get the "unavailable" refusal.
	forwardRegistrar ForwardRegistrar
	channelBuf       *ChannelBuffer // nil if no managed channels configured
	// dialogLister fetches MTProto dialogs for /dialogs (Stage 8 A2);
	// nil => MTProto is not configured and /dialogs degrades to a refusal.
	dialogLister DialogLister
	// callbackHandler receives callbacks whose data matches no known prefix
	// (ad:/dp:) — the skeleton Stage 8 D extends into the drill-down/topic
	// dispatcher. It must answer the callback itself.
	callbackHandler func(ctx context.Context, q *models.CallbackQuery)

	// Dialogs snapshot + rate limit (plan §4.6: at most one real GetDialogs
	// per 30s; repeated calls reuse the last successful snapshot).
	dialogsMu        sync.Mutex
	dialogsSnapshot  []mtproto.PeerInfo
	dialogsFetchedAt time.Time
	dialogsCached    bool

	// deliverFn is a test seam overriding Deliver's transport; nil => b.api.
	deliverFn func(ctx context.Context, chatID, text string) error
	// Test seams for the callback transport; nil => b.api (no-op when api is
	// nil, i.e. in unit tests without a library client).
	nowFn  func() time.Time
	sendFn func(ctx context.Context, chatID int64, text string, kb *models.InlineKeyboardMarkup) error
	ackFn  func(ctx context.Context, callbackQueryID, text string) error
	editFn func(ctx context.Context, chatID int64, messageID int, text string, kb *models.InlineKeyboardMarkup) error
	log    *slog.Logger
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

// WithSourceAdder wires /addsource to a closure that validates (trial collect)
// and persists a new source, returning its id and the number of posts found.
func WithSourceAdder(fn func(ctx context.Context, kind, ref string) (int64, int, error)) Option {
	return func(b *Bot) { b.addSource = fn }
}

// WithSourceRemover wires /removesource to a closure that disables a source by
// id, returning whether such a row existed.
func WithSourceRemover(fn func(ctx context.Context, id int64) (bool, error)) Option {
	return func(b *Bot) { b.removeSource = fn }
}

// WithChannelBuffer wires managed-channel posts into a shared per-key buffer
// drained by ManagedSource. When nil, channel posts are ignored.
func WithChannelBuffer(buf *ChannelBuffer) Option {
	return func(b *Bot) { b.channelBuf = buf }
}

// WithForwardRegistrar wires forwarded messages from the owner to a
// source-registration closure (Stage 8 A1). The closure receives the
// extracted origin and returns the reply text shown to the owner.
func WithForwardRegistrar(fn ForwardRegistrar) Option {
	return func(b *Bot) { b.forwardRegistrar = fn }
}

// WithDialogLister wires /dialogs to the MTProto dialogs fetcher (Stage 8
// A2). Without it /dialogs answers that MTProto is not configured.
func WithDialogLister(fn DialogLister) Option {
	return func(b *Bot) { b.dialogLister = fn }
}

// WithCallbackHandler wires callbacks whose data matches no known dialogs
// prefix (ad:/dp:) to fn — the skeleton Stage 8 D extends for drill-down
// (L1/L2/L3) and topic (ts/tl/tn) dispatching. fn must ack the callback.
func WithCallbackHandler(fn func(ctx context.Context, q *models.CallbackQuery)) Option {
	return func(b *Bot) { b.callbackHandler = fn }
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
	if b.deliverFn != nil {
		return b.deliverFn(ctx, chatID, text)
	}
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
