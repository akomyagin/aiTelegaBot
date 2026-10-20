package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Peer is one row of mtproto_peers: cached MTProto peer metadata (Stage 8,
// migration v2). The access_hash of a private channel cannot be derived from
// its Bot API id — it is obtained via messages.getDialogs and cached here so
// private sources survive restarts.
//
// Security note: access_hash is sensitive metadata; never log its value.
type Peer struct {
	MTProtoID  int64
	AccessHash int64
	Title      string
	Username   string // "" for private peers
	UpdatedAt  string // RFC3339 UTC
}

// UpsertPeer inserts or refreshes a cached peer.
func (s *SQLiteStore) UpsertPeer(ctx context.Context, peerID, accessHash int64, title, username string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO mtproto_peers(peer_id, access_hash, title, username, updated_at)
		VALUES(?,?,?,?,?)
		ON CONFLICT(peer_id) DO UPDATE SET
		    access_hash = excluded.access_hash,
		    title       = excluded.title,
		    username    = excluded.username,
		    updated_at  = excluded.updated_at`,
		peerID, accessHash, title, username, now)
	if err != nil {
		return fmt.Errorf("upsert peer %d: %w", peerID, err)
	}
	return nil
}

// GetPeer reads a cached peer. ok=false when the peer is unknown (the caller
// typically responds with a dialogs refresh).
func (s *SQLiteStore) GetPeer(ctx context.Context, peerID int64) (Peer, bool, error) {
	var p Peer
	err := s.db.QueryRowContext(ctx,
		"SELECT peer_id, access_hash, title, username, updated_at FROM mtproto_peers WHERE peer_id=?",
		peerID).Scan(&p.MTProtoID, &p.AccessHash, &p.Title, &p.Username, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return Peer{}, false, nil
	}
	if err != nil {
		return Peer{}, false, fmt.Errorf("get peer %d: %w", peerID, err)
	}
	return p, true, nil
}
