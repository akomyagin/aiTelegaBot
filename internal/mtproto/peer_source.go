package mtproto

import (
	"context"
	"strconv"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
)

// PeerSource reads the history of a numeric peer (private channel/supergroup,
// MTProto id) via the cached access_hash (Stage 8 A1). Items carry t.me deep
// links: https://t.me/<username>/<msg> when the peer has a username,
// https://t.me/c/<X>/<msg> otherwise. feed.Item.Source is the Bot API
// "-100X" string — the canonical tg_mtproto ref in storage (plan §4.6).
type PeerSource struct {
	name      string
	mtprotoID int64
	limit     int
	client    *Client // shared; must not be closed by the source
}

// NewPeerSource builds a PeerSource for MTProto id mtprotoID (the X of
// "-100X"). The client is shared and must outlive the source.
func NewPeerSource(name string, mtprotoID int64, limit int, client *Client) *PeerSource {
	return &PeerSource{name: name, mtprotoID: mtprotoID, limit: limit, client: client}
}

// Name returns the human-readable source name.
func (s *PeerSource) Name() string { return s.name }

// Collect returns the last s.limit messages of the peer. FloodWait/backoff and
// the cache-miss dialogs refresh live in Client.GetHistoryByPeer.
func (s *PeerSource) Collect(ctx context.Context) ([]feed.Item, error) {
	msgs, err := s.client.GetHistoryByPeer(ctx, s.mtprotoID, s.limit)
	if err != nil {
		return nil, err
	}
	username := s.client.peerUsername(ctx, s.mtprotoID)
	source := strconv.FormatInt(ToBotAPIID(s.mtprotoID), 10)
	items := make([]feed.Item, 0, len(msgs))
	for _, msg := range msgs {
		// Single link build: the cached username (when known) yields the
		// public t.me/<username> form, otherwise the private t.me/c/<X> form.
		items = append(items, messageToFeedItem(msg, source, deepLinkURL(username, s.mtprotoID, msg.ID)))
	}
	return items, nil
}

// Compile-time check: PeerSource is a feed.Source.
var _ feed.Source = (*PeerSource)(nil)
