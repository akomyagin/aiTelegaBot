package mtproto

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gotd/td/tg"
)

func TestMemPeerCacheRoundtrip(t *testing.T) {
	ctx := context.Background()
	c := NewMemPeerCache()

	_, ok, err := c.AccessHash(ctx, 1234567890)
	if err != nil {
		t.Fatalf("AccessHash unknown: %v", err)
	}
	if ok {
		t.Fatal("AccessHash on empty cache returned ok=true")
	}

	p := PeerInfo{
		MTProtoID:  1234567890,
		BotAPIID:   ToBotAPIID(1234567890),
		AccessHash: 424242,
		Title:      "My Channel",
		Username:   "chan",
		Kind:       "channel",
	}
	if err := c.Store(ctx, p); err != nil {
		t.Fatalf("Store: %v", err)
	}
	hash, ok, err := c.AccessHash(ctx, 1234567890)
	if err != nil || !ok || hash != 424242 {
		t.Fatalf("AccessHash = (%d, %v, %v), want (424242, true, nil)", hash, ok, err)
	}
	got, ok, err := c.Get(ctx, 1234567890)
	if err != nil || !ok || got != p {
		t.Fatalf("Get = (%+v, %v, %v), want the stored peer", got, ok, err)
	}

	// Overwrite refreshes in place.
	p.AccessHash = 434343
	if err := c.Store(ctx, p); err != nil {
		t.Fatalf("Store refresh: %v", err)
	}
	if hash, _, _ := c.AccessHash(ctx, 1234567890); hash != 434343 {
		t.Errorf("AccessHash after refresh = %d, want 434343", hash)
	}
}

// TestResolveInputPeer covers the network-free resolve logic (plan A1):
// a cached peer resolves to InputPeerChannel; a miss reports errPeerUnknown;
// a nil cache produces an explicit, understandable error.
func TestResolveInputPeer(t *testing.T) {
	ctx := context.Background()

	peer, err := resolveInputPeer(ctx, nil, 1234567890)
	if err == nil {
		t.Fatal("nil cache: expected error")
	}
	if peer != nil {
		t.Errorf("nil cache: expected nil peer")
	}
	if !strings.Contains(err.Error(), "no peer cache") {
		t.Errorf("nil cache error = %q, want a message about the missing peer cache", err)
	}

	c := NewMemPeerCache()
	_, err = resolveInputPeer(ctx, c, 1234567890)
	if !errors.Is(err, errPeerUnknown) {
		t.Fatalf("cache miss: err = %v, want errPeerUnknown", err)
	}

	if err := c.Store(ctx, PeerInfo{MTProtoID: 1234567890, AccessHash: 424242}); err != nil {
		t.Fatalf("Store: %v", err)
	}
	peer, err = resolveInputPeer(ctx, c, 1234567890)
	if err != nil {
		t.Fatalf("resolve with cache: %v", err)
	}
	ch, ok := peer.(*tg.InputPeerChannel)
	if !ok {
		t.Fatalf("peer type = %T, want *tg.InputPeerChannel", peer)
	}
	if ch.ChannelID != 1234567890 || ch.AccessHash != 424242 {
		t.Errorf("InputPeerChannel = %+v, want id/hash from the cache", ch)
	}
}

func TestDeepLinkURL(t *testing.T) {
	tests := []struct {
		name      string
		username  string
		mtprotoID int64
		msgID     int
		want      string
	}{
		{"public", "chan", 1234567890, 42, "https://t.me/chan/42"},
		{"private", "", 1234567890, 42, "https://t.me/c/1234567890/42"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := deepLinkURL(tt.username, tt.mtprotoID, tt.msgID); got != tt.want {
				t.Errorf("deepLinkURL(%q, %d, %d) = %q, want %q",
					tt.username, tt.mtprotoID, tt.msgID, got, tt.want)
			}
		})
	}
}
