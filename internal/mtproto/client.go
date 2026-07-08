package mtproto

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/gotd/contrib/middleware/floodwait"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

const (
	historyBaseDelay = 2 * time.Second
	historyMaxDelay  = 120 * time.Second
	historyMaxTries  = 3
)

// MTProtoConfig holds the credentials and session settings for the client.
type MTProtoConfig struct {
	AppID       int
	AppHash     string
	SessionPath string
	SessionKey  []byte // 32-byte AES-256 key; empty => plaintext session
}

// Client wraps a gotd/td telegram.Client for interactive login (Этап 6) and
// channel history reading via messages.getHistory (Этап 7).
type Client struct {
	cfg     MTProtoConfig
	storage *EncryptedFileStorage
	client  *telegram.Client
}

// NewClient builds a Client. It fails only on an obviously invalid session key
// length; credential validation happens when the client actually connects.
func NewClient(cfg MTProtoConfig) (*Client, error) {
	if len(cfg.SessionKey) == 0 {
		slog.Warn("MTProto session stored unencrypted — set MTPROTO_SESSION_KEY for production")
	}
	storage, err := NewEncryptedFileStorage(cfg.SessionPath, cfg.SessionKey)
	if err != nil {
		return nil, err
	}
	client := telegram.NewClient(cfg.AppID, cfg.AppHash, telegram.Options{
		SessionStorage: &gotdStorage{inner: storage},
		// FloodWait discipline: the simple waiter transparently blocks and
		// retries on FLOOD_WAIT for every API call. GetHistory adds its own
		// bounded exponential backoff on top.
		Middlewares: []telegram.Middleware{floodwait.NewSimpleWaiter()},
	})
	return &Client{cfg: cfg, storage: storage, client: client}, nil
}

// gotdStorage adapts EncryptedFileStorage to gotd's expectation that a missing
// session is reported as session.ErrNotFound (our store returns (nil, nil) so
// the plain roundtrip tests stay simple).
type gotdStorage struct {
	inner *EncryptedFileStorage
}

func (g *gotdStorage) LoadSession(ctx context.Context) ([]byte, error) {
	data, err := g.inner.LoadSession(ctx)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, session.ErrNotFound
	}
	return data, nil
}

func (g *gotdStorage) StoreSession(ctx context.Context, data []byte) error {
	return g.inner.StoreSession(ctx, data)
}

// IsAuthorized reports whether the stored session is a logged-in account.
func (c *Client) IsAuthorized(ctx context.Context) (bool, error) {
	var authorized bool
	err := c.client.Run(ctx, func(ctx context.Context) error {
		status, err := c.client.Auth().Status(ctx)
		if err != nil {
			return err
		}
		authorized = status.Authorized
		return nil
	})
	if err != nil {
		return false, err
	}
	return authorized, nil
}

// Login performs an interactive authorization: it prompts for phone, the code,
// and (if 2FA is enabled) the password on stdin, then persists the session.
func (c *Client) Login(ctx context.Context) error {
	return c.client.Run(ctx, func(ctx context.Context) error {
		flow := auth.NewFlow(
			stdinAuth{},
			auth.SendCodeOptions{},
		)
		if err := c.client.Auth().IfNecessary(ctx, flow); err != nil {
			return fmt.Errorf("mtproto: auth flow: %w", err)
		}
		self, err := c.client.Self(ctx)
		if err != nil {
			return fmt.Errorf("mtproto: fetch self: %w", err)
		}
		slog.Info("MTProto login successful", "user_id", self.ID, "username", self.Username)
		return nil
	})
}

// Login is a convenience entrypoint used by the `bot login` subcommand: it
// builds a client from cfg and runs the interactive flow.
func Login(ctx context.Context, cfg MTProtoConfig) error {
	if cfg.AppID == 0 || cfg.AppHash == "" {
		return fmt.Errorf("mtproto: MTPROTO_APP_ID and MTPROTO_APP_HASH must be set for login")
	}
	c, err := NewClient(cfg)
	if err != nil {
		return err
	}
	return c.Login(ctx)
}

// GetHistory returns the last limit messages from channel (a username, with or
// without a leading '@'). Numeric "-100..." peer IDs are not supported yet.
//
// Each call opens a short-lived MTProto session (client.Run), resolves the
// channel, fetches history, and closes — this is fine for the once-a-day digest
// workload. FloodWait is handled two ways: the floodwait middleware waits
// transparently, and on top of that this method retries with exponential
// backoff + jitter (up to historyMaxTries) if a FLOOD_WAIT still surfaces.
func (c *Client) GetHistory(ctx context.Context, channel string, limit int) ([]tg.Message, error) {
	channel = strings.TrimPrefix(strings.TrimSpace(channel), "@")
	if channel == "" {
		return nil, fmt.Errorf("mtproto: empty channel")
	}
	if strings.HasPrefix(channel, "-100") {
		return nil, fmt.Errorf("mtproto: numeric peer IDs (%q) are not supported; use the @username", channel)
	}

	var out []tg.Message
	err := c.client.Run(ctx, func(ctx context.Context) error {
		msgs, err := c.getHistoryWithRetry(ctx, channel, limit)
		if err != nil {
			return err
		}
		out = msgs
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// getHistoryWithRetry runs inside client.Run: it resolves the channel and calls
// messages.getHistory, retrying on FloodWait with bounded backoff.
func (c *Client) getHistoryWithRetry(ctx context.Context, username string, limit int) ([]tg.Message, error) {
	api := c.client.API()
	var lastErr error
	for attempt := 0; attempt < historyMaxTries; attempt++ {
		msgs, err := fetchHistory(ctx, api, username, limit)
		if err == nil {
			return msgs, nil
		}
		if d, ok := tgerr.AsFloodWait(err); ok {
			lastErr = err
			wait := d + historyBackoff(attempt)
			if wait > historyMaxDelay {
				wait = historyMaxDelay
			}
			slog.Warn("mtproto getHistory flood wait", "channel", username, "attempt", attempt, "wait", wait)
			if werr := sleepCtx(ctx, wait); werr != nil {
				return nil, werr
			}
			continue
		}
		return nil, err
	}
	return nil, fmt.Errorf("mtproto: getHistory %q: flood wait retries exhausted: %w", username, lastErr)
}

// fetchHistory resolves username to an input peer and returns its messages.
func fetchHistory(ctx context.Context, api *tg.Client, username string, limit int) ([]tg.Message, error) {
	resolved, err := api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: username})
	if err != nil {
		return nil, fmt.Errorf("mtproto: resolve %q: %w", username, err)
	}
	peer, err := channelPeer(resolved)
	if err != nil {
		return nil, err
	}
	res, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
		Peer:  peer,
		Limit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("mtproto: getHistory %q: %w", username, err)
	}
	msgClasses, ok := res.(interface{ GetMessages() []tg.MessageClass })
	if !ok {
		return nil, fmt.Errorf("mtproto: unexpected messages type %T for %q", res, username)
	}
	var out []tg.Message
	for _, mc := range msgClasses.GetMessages() {
		if m, ok := mc.(*tg.Message); ok {
			out = append(out, *m)
		}
	}
	return out, nil
}

// channelPeer picks the resolved channel and builds an input peer with its
// access hash.
func channelPeer(resolved *tg.ContactsResolvedPeer) (tg.InputPeerClass, error) {
	for _, chat := range resolved.Chats {
		if ch, ok := chat.(*tg.Channel); ok {
			return &tg.InputPeerChannel{ChannelID: ch.ID, AccessHash: ch.AccessHash}, nil
		}
	}
	return nil, fmt.Errorf("mtproto: resolved peer is not a channel")
}

// historyBackoff returns base*2^attempt (capped) plus jitter in [0, d/2).
func historyBackoff(attempt int) time.Duration {
	d := historyBaseDelay << attempt
	if d <= 0 || d > historyMaxDelay {
		d = historyMaxDelay
	}
	return d + time.Duration(rand.Int63n(int64(d/2)))
}

// sleepCtx waits for d or returns early if ctx is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// stdinAuth implements auth.UserAuthenticator by prompting on stdin. Sign-up of
// new accounts is not supported (this is a login for an existing account).
type stdinAuth struct{}

func (stdinAuth) SignUp(_ context.Context) (auth.UserInfo, error) {
	return auth.UserInfo{}, fmt.Errorf("mtproto: sign-up not supported; use an existing account")
}

func (stdinAuth) AcceptTermsOfService(_ context.Context, _ tg.HelpTermsOfService) error {
	return fmt.Errorf("mtproto: sign-up not supported")
}

func (stdinAuth) Phone(_ context.Context) (string, error) {
	return prompt("Phone (international format, e.g. +79991234567): ")
}

func (stdinAuth) Password(_ context.Context) (string, error) {
	return prompt("2FA password: ")
}

func (stdinAuth) Code(_ context.Context, _ *tg.AuthSentCode) (string, error) {
	return prompt("Confirmation code: ")
}

func prompt(label string) (string, error) {
	fmt.Fprint(os.Stderr, label)
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}
