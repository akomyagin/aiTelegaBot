package mtproto

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"strings"
	"sync"
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
	// defaultDialogsLimit caps one messages.getDialogs call (plan §4.6:
	// a single call per refresh; the daily workload needs only the head).
	defaultDialogsLimit = 100
	// peerRefreshMinInterval rate-limits dialogs refreshes (plan §4.6/§7 —
	// FloodWait mitigation for the secondary account): N missing/stale peers
	// in one digest run must trigger at most ONE messages.getDialogs per
	// interval, not N (review M2).
	peerRefreshMinInterval = 30 * time.Second
)

// MTProtoConfig holds the credentials and session settings for the client.
type MTProtoConfig struct {
	AppID       int
	AppHash     string
	SessionPath string
	SessionKey  []byte // 32-byte AES-256 key; empty => plaintext session
}

// Client wraps a gotd/td telegram.Client for interactive login (Этап 6),
// channel history reading via messages.getHistory (Этап 7) and numeric-peer
// access via the cached access_hash (Stage 8: GetDialogs/GetHistoryByPeer).
type Client struct {
	cfg       MTProtoConfig
	storage   *EncryptedFileStorage
	client    *telegram.Client
	peerCache PeerCache // nil => numeric peers cannot be resolved (Stage 8)

	// mu guards lastPeerRefresh — the in-memory rate limit of
	// refreshPeerCache (review M2, plan §4.6/§7).
	mu              sync.Mutex
	lastPeerRefresh time.Time // last SUCCESSFUL dialogs refresh
}

// Option configures a Client.
type Option func(*Client)

// WithPeerCache injects the access_hash cache used by GetHistoryByPeer and
// filled by GetDialogs (Stage 8). Without a cache, numeric peers cannot be
// resolved and GetHistoryByPeer fails with an explicit error.
func WithPeerCache(pc PeerCache) Option {
	return func(c *Client) { c.peerCache = pc }
}

// NewClient builds a Client. It fails only on an obviously invalid session key
// length; credential validation happens when the client actually connects.
func NewClient(cfg MTProtoConfig, opts ...Option) (*Client, error) {
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
	c := &Client{cfg: cfg, storage: storage, client: client}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
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

// GetDialogs fetches up to limit dialogs of the logged-in account (ONE
// messages.getDialogs call, plan §4.6), normalizes each into PeerInfo and
// stores the channel/supergroup peers in the PeerCache (when configured).
// FloodWait is handled by the middleware plus a bounded backoff on top, the
// same pattern as getHistoryWithRetry.
func (c *Client) GetDialogs(ctx context.Context, limit int) ([]PeerInfo, error) {
	if limit <= 0 {
		limit = defaultDialogsLimit
	}
	var out []PeerInfo
	err := c.client.Run(ctx, func(ctx context.Context) error {
		peers, err := c.fetchDialogsWithRetry(ctx, limit)
		if err != nil {
			return err
		}
		out = peers
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// fetchDialogsWithRetry runs inside client.Run and persists the fetched peers
// into the cache.
func (c *Client) fetchDialogsWithRetry(ctx context.Context, limit int) ([]PeerInfo, error) {
	api := c.client.API()
	var lastErr error
	for attempt := 0; attempt < historyMaxTries; attempt++ {
		peers, err := fetchDialogs(ctx, api, limit)
		if err == nil {
			c.storePeers(ctx, peers)
			return peers, nil
		}
		if d, ok := tgerr.AsFloodWait(err); ok {
			lastErr = err
			wait := d + historyBackoff(attempt)
			if wait > historyMaxDelay {
				wait = historyMaxDelay
			}
			slog.Warn("mtproto getDialogs flood wait", "attempt", attempt, "wait", wait)
			if werr := sleepCtx(ctx, wait); werr != nil {
				return nil, werr
			}
			continue
		}
		return nil, err
	}
	return nil, fmt.Errorf("mtproto: getDialogs: flood wait retries exhausted: %w", lastErr)
}

// storePeers saves channel/supergroup peers into the cache. Basic chats and
// users are NOT stored: the cache is keyed by peer id alone, and only
// channel-type ids are resolved through it (no cross-kind id collisions).
// A store failure is logged (without the access_hash), never fatal — the next
// refresh retries.
func (c *Client) storePeers(ctx context.Context, peers []PeerInfo) {
	if c.peerCache == nil {
		return
	}
	for _, p := range peers {
		if p.Kind != "channel" && p.Kind != "supergroup" {
			continue
		}
		if err := c.peerCache.Store(ctx, p); err != nil {
			slog.Warn("mtproto: store peer failed", "peer_id", p.MTProtoID, "err", err)
		}
	}
}

// fetchDialogs issues messages.getDialogs and normalizes the result.
func fetchDialogs(ctx context.Context, api *tg.Client, limit int) ([]PeerInfo, error) {
	res, err := api.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{Limit: limit})
	if err != nil {
		return nil, fmt.Errorf("mtproto: getDialogs: %w", err)
	}
	dialogs, ok := res.(*tg.MessagesDialogs)
	if !ok {
		// MessagesDialogsNotModified carries no data — treat as empty.
		return nil, nil
	}
	chats := make(map[int64]tg.ChatClass, len(dialogs.Chats))
	for _, ch := range dialogs.Chats {
		switch v := ch.(type) {
		case *tg.Channel:
			chats[v.ID] = v
		case *tg.Chat:
			chats[v.ID] = v
		}
	}
	users := make(map[int64]*tg.User, len(dialogs.Users))
	for _, u := range dialogs.Users {
		if v, ok := u.(*tg.User); ok {
			users[v.ID] = v
		}
	}
	var out []PeerInfo
	for _, dc := range dialogs.Dialogs {
		d, ok := dc.(*tg.Dialog)
		if !ok {
			continue
		}
		switch p := d.Peer.(type) {
		case *tg.PeerChannel:
			ch, ok := chats[p.ChannelID].(*tg.Channel)
			if !ok {
				continue
			}
			kind := "channel"
			if ch.Megagroup {
				kind = "supergroup"
			}
			out = append(out, PeerInfo{
				MTProtoID:  ch.ID,
				BotAPIID:   ToBotAPIID(ch.ID),
				AccessHash: ch.AccessHash,
				Title:      ch.Title,
				Username:   ch.Username,
				Kind:       kind,
			})
		case *tg.PeerChat:
			if ch, ok := chats[p.ChatID].(*tg.Chat); ok {
				// Basic groups have no access_hash and no Bot API channel form.
				out = append(out, PeerInfo{MTProtoID: ch.ID, Title: ch.Title, Kind: "chat"})
			}
		case *tg.PeerUser:
			if u, ok := users[p.UserID]; ok {
				out = append(out, PeerInfo{
					MTProtoID:  u.ID,
					AccessHash: u.AccessHash,
					Title:      strings.TrimSpace(u.FirstName + " " + u.LastName),
					Username:   u.Username,
					Kind:       "user",
				})
			}
		}
	}
	return out, nil
}

// GetHistoryByPeer returns the last limit messages of a numeric peer (MTProto
// id X of the Bot API "-100X" form). The access_hash comes from the PeerCache;
// on a cache miss the cache is refreshed ONCE via messages.getDialogs and the
// lookup retried. A STALE cached access_hash (CHANNEL_INVALID/CHANNEL_PRIVATE
// from getHistory — the chat was e.g. recreated or migrated) triggers exactly
// ONE dialogs refresh + ONE retry, never a loop (plan §4.6/§7, review M1); if
// that still fails, the error is the explicit "not a member of the chat".
func (c *Client) GetHistoryByPeer(ctx context.Context, mtprotoID int64, limit int) ([]tg.Message, error) {
	if mtprotoID <= 0 {
		return nil, fmt.Errorf("mtproto: invalid peer id %d", mtprotoID)
	}
	peer, err := resolveInputPeer(ctx, c.peerCache, mtprotoID)
	if errors.Is(err, errPeerUnknown) {
		if rerr := c.refreshPeerCache(ctx); rerr != nil {
			return nil, fmt.Errorf("mtproto: peer %d unknown and dialogs refresh failed: %w", mtprotoID, rerr)
		}
		peer, err = resolveInputPeer(ctx, c.peerCache, mtprotoID)
	}
	if err != nil {
		return nil, err
	}

	notMemberErr := func(cause error) error {
		return fmt.Errorf("mtproto: account is not a member of chat %d (or the chat is unavailable): %w", mtprotoID, cause)
	}

	out, err := c.runHistoryByPeer(ctx, peer, mtprotoID, limit)
	if errors.Is(err, errPeerStale) {
		// The cached access_hash is stale: one refresh, one repeat — bounded.
		slog.Info("mtproto: stale access_hash, refreshing dialogs once", "peer_id", mtprotoID)
		if rerr := c.refreshPeerCache(ctx); rerr != nil {
			return nil, notMemberErr(rerr)
		}
		peer, perr := resolveInputPeer(ctx, c.peerCache, mtprotoID)
		if perr != nil {
			// Refreshed dialogs do not contain the peer — the account is
			// (no longer) in this chat.
			return nil, notMemberErr(perr)
		}
		out, err = c.runHistoryByPeer(ctx, peer, mtprotoID, limit)
	}
	if err != nil {
		if errors.Is(err, errPeerStale) {
			return nil, notMemberErr(err)
		}
		return nil, err
	}
	return out, nil
}

// runHistoryByPeer performs one getHistory attempt sequence (bounded FloodWait
// backoff inside) in its own short-lived client.Run session.
func (c *Client) runHistoryByPeer(ctx context.Context, peer tg.InputPeerClass, mtprotoID int64, limit int) ([]tg.Message, error) {
	var out []tg.Message
	err := c.client.Run(ctx, func(ctx context.Context) error {
		msgs, err := c.historyByPeerWithRetry(ctx, peer, mtprotoID, limit)
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

// refreshPeerCache performs one dialogs fetch in its own client.Run and stores
// the result (fetchDialogsWithRetry persists into the cache). Rate-limited: a
// successful refresh less than peerRefreshMinInterval ago skips the network
// and returns nil — the repeated resolveInputPeer then reads the just-filled
// cache (plan §4.6/§7, review M2). Failed refreshes are NOT remembered, so the
// next caller retries.
func (c *Client) refreshPeerCache(ctx context.Context) error {
	if c.peerCache == nil {
		return errors.New("mtproto: no peer cache configured")
	}
	c.mu.Lock()
	recent := !c.lastPeerRefresh.IsZero() && time.Since(c.lastPeerRefresh) < peerRefreshMinInterval
	c.mu.Unlock()
	if recent {
		return nil
	}

	err := c.client.Run(ctx, func(ctx context.Context) error {
		_, err := c.fetchDialogsWithRetry(ctx, defaultDialogsLimit)
		return err
	})
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.lastPeerRefresh = time.Now()
	c.mu.Unlock()
	return nil
}

// historyByPeerWithRetry runs inside client.Run: messages.getHistory on a
// resolved input peer, with the same bounded FloodWait backoff as
// getHistoryWithRetry.
func (c *Client) historyByPeerWithRetry(ctx context.Context, peer tg.InputPeerClass, mtprotoID int64, limit int) ([]tg.Message, error) {
	api := c.client.API()
	var lastErr error
	for attempt := 0; attempt < historyMaxTries; attempt++ {
		res, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
			Peer:  peer,
			Limit: limit,
		})
		if err != nil {
			if tgerr.Is(err, "CHANNEL_INVALID", "CHANNEL_PRIVATE") {
				// Sentinel for GetHistoryByPeer: the cached access_hash may be
				// stale — worth one dialogs refresh + retry (plan §4.6).
				return nil, fmt.Errorf("%w: peer %d: %w", errPeerStale, mtprotoID, err)
			}
			if d, ok := tgerr.AsFloodWait(err); ok {
				lastErr = err
				wait := d + historyBackoff(attempt)
				if wait > historyMaxDelay {
					wait = historyMaxDelay
				}
				slog.Warn("mtproto getHistoryByPeer flood wait", "peer_id", mtprotoID, "attempt", attempt, "wait", wait)
				if werr := sleepCtx(ctx, wait); werr != nil {
					return nil, werr
				}
				continue
			}
			return nil, fmt.Errorf("mtproto: getHistory peer %d: %w", mtprotoID, err)
		}
		return messagesFromResponse(res, fmt.Sprintf("peer %d", mtprotoID))
	}
	return nil, fmt.Errorf("mtproto: getHistory peer %d: flood wait retries exhausted: %w", mtprotoID, lastErr)
}

// messagesFromResponse extracts plain messages from a messages.getHistory
// response. label identifies the request in errors.
func messagesFromResponse(res tg.MessagesMessagesClass, label string) ([]tg.Message, error) {
	msgClasses, ok := res.(interface{ GetMessages() []tg.MessageClass })
	if !ok {
		return nil, fmt.Errorf("mtproto: unexpected messages type %T for %s", res, label)
	}
	var out []tg.Message
	for _, mc := range msgClasses.GetMessages() {
		if m, ok := mc.(*tg.Message); ok {
			out = append(out, *m)
		}
	}
	return out, nil
}

// peerUsername returns the cached username of a peer ("" when unknown or no
// cache is configured) — used to prefer public t.me deep links.
func (c *Client) peerUsername(ctx context.Context, mtprotoID int64) string {
	if c.peerCache == nil {
		return ""
	}
	p, ok, err := c.peerCache.Get(ctx, mtprotoID)
	if err != nil || !ok {
		return ""
	}
	return p.Username
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
