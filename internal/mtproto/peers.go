package mtproto

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/gotd/td/tg"
)

// PeerInfo is normalized metadata of one MTProto peer (channel, supergroup,
// basic chat or user) obtained from messages.getDialogs. The access_hash of a
// private peer cannot be derived from its Bot API id, so peers are cached
// (Stage 8, table mtproto_peers).
type PeerInfo struct {
	MTProtoID  int64 // X of the Bot API "-100X" form (channels/supergroups)
	BotAPIID   int64 // -100X; 0 for kinds without a Bot API channel form
	AccessHash int64 // sensitive: never log the value
	Title      string
	Username   string // "" for private peers
	Kind       string // "channel" | "supergroup" | "chat" | "user"
}

// PeerCache persists peer metadata between runs. Implemented over storage by
// the app package (storagePeerCache); mtproto depends only on this interface,
// not on storage.
type PeerCache interface {
	// AccessHash returns the cached access_hash; ok=false on a cache miss.
	AccessHash(ctx context.Context, mtprotoID int64) (hash int64, ok bool, err error)
	// Store inserts or refreshes one peer.
	Store(ctx context.Context, p PeerInfo) error
	// Get returns the full cached peer; ok=false on a cache miss. Used for
	// deep-link building (username lookup).
	Get(ctx context.Context, mtprotoID int64) (PeerInfo, bool, error)
}

// MemPeerCache is an in-memory PeerCache for tests. Safe for concurrent use.
type MemPeerCache struct {
	mu    sync.Mutex
	peers map[int64]PeerInfo
}

// NewMemPeerCache builds an empty in-memory cache.
func NewMemPeerCache() *MemPeerCache {
	return &MemPeerCache{peers: make(map[int64]PeerInfo)}
}

// AccessHash implements PeerCache.
func (c *MemPeerCache) AccessHash(_ context.Context, mtprotoID int64) (int64, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.peers[mtprotoID]
	return p.AccessHash, ok, nil
}

// Store implements PeerCache.
func (c *MemPeerCache) Store(_ context.Context, p PeerInfo) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.peers == nil {
		c.peers = make(map[int64]PeerInfo)
	}
	c.peers[p.MTProtoID] = p
	return nil
}

// Get implements PeerCache.
func (c *MemPeerCache) Get(_ context.Context, mtprotoID int64) (PeerInfo, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.peers[mtprotoID]
	return p, ok, nil
}

// errPeerUnknown signals a cache miss in resolveInputPeer; the client reacts
// with a single dialogs refresh before giving up.
var errPeerUnknown = errors.New("peer is not in the cache (access_hash unknown)")

// errPeerStale signals CHANNEL_INVALID/CHANNEL_PRIVATE on a cached
// access_hash (chat recreated/migrated); GetHistoryByPeer reacts with exactly
// one dialogs refresh and one retry (plan §4.6, review M1).
var errPeerStale = errors.New("peer access_hash is stale (CHANNEL_INVALID/CHANNEL_PRIVATE)")

// resolveInputPeer builds an InputPeerChannel from the cached access_hash.
// Pure (no network) so it is unit-testable with MemPeerCache (plan A1).
func resolveInputPeer(ctx context.Context, cache PeerCache, mtprotoID int64) (tg.InputPeerClass, error) {
	if cache == nil {
		return nil, fmt.Errorf("mtproto: no peer cache configured; cannot resolve peer %d (access_hash unknown)", mtprotoID)
	}
	hash, ok, err := cache.AccessHash(ctx, mtprotoID)
	if err != nil {
		return nil, fmt.Errorf("mtproto: peer cache lookup %d: %w", mtprotoID, err)
	}
	if !ok {
		return nil, fmt.Errorf("mtproto: peer %d: %w", mtprotoID, errPeerUnknown)
	}
	return &tg.InputPeerChannel{ChannelID: mtprotoID, AccessHash: hash}, nil
}

// deepLinkURL builds the t.me deep link for one message: peers with a username
// get https://t.me/<username>/<msg>, private ones https://t.me/c/<X>/<msg>
// (Telegram clients resolve the numeric form for logged-in accounts).
func deepLinkURL(username string, mtprotoID int64, msgID int) string {
	if username != "" {
		return fmt.Sprintf("https://t.me/%s/%d", username, msgID)
	}
	return fmt.Sprintf("https://t.me/c/%d/%d", mtprotoID, msgID)
}
