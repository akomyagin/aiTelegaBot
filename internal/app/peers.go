package app

import (
	"context"

	"github.com/akomyagin/aiTelegaBot/internal/mtproto"
	"github.com/akomyagin/aiTelegaBot/internal/storage"
)

// storagePeerCache adapts the SQLite store to mtproto.PeerCache (Stage 8 A1):
// the access_hash cache must survive restarts, so it lives in the
// mtproto_peers table. The adapter sits here — not in the mtproto package —
// so mtproto keeps zero knowledge of storage (plan §4.6).
//
// Note: Get does not restore Kind (the table does not store it); the field is
// only needed for dialog listings, and deep-link building uses Username.
type storagePeerCache struct {
	store *storage.SQLiteStore
}

// Compile-time check: storagePeerCache implements mtproto.PeerCache.
var _ mtproto.PeerCache = storagePeerCache{}

// AccessHash implements mtproto.PeerCache.
func (c storagePeerCache) AccessHash(ctx context.Context, mtprotoID int64) (int64, bool, error) {
	p, ok, err := c.store.GetPeer(ctx, mtprotoID)
	if err != nil || !ok {
		return 0, false, err
	}
	return p.AccessHash, true, nil
}

// Store implements mtproto.PeerCache.
func (c storagePeerCache) Store(ctx context.Context, p mtproto.PeerInfo) error {
	return c.store.UpsertPeer(ctx, p.MTProtoID, p.AccessHash, p.Title, p.Username)
}

// Get implements mtproto.PeerCache.
func (c storagePeerCache) Get(ctx context.Context, mtprotoID int64) (mtproto.PeerInfo, bool, error) {
	p, ok, err := c.store.GetPeer(ctx, mtprotoID)
	if err != nil || !ok {
		return mtproto.PeerInfo{}, false, err
	}
	return mtproto.PeerInfo{
		MTProtoID:  p.MTProtoID,
		BotAPIID:   mtproto.ToBotAPIID(p.MTProtoID),
		AccessHash: p.AccessHash,
		Title:      p.Title,
		Username:   p.Username,
	}, true, nil
}
