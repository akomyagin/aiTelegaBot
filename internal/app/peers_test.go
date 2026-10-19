package app

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/akomyagin/aiTelegaBot/internal/mtproto"
	"github.com/akomyagin/aiTelegaBot/internal/storage"
)

// TestStoragePeerCacheRoundtrip exercises the mtproto.PeerCache adapter over
// a real temporary SQLite (no network): miss, store, hit, refresh, full Get.
func TestStoragePeerCacheRoundtrip(t *testing.T) {
	st, err := storage.Open(filepath.Join(t.TempDir(), "peers.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	c := storagePeerCache{store: st}

	_, ok, err := c.AccessHash(ctx, 1234567890)
	if err != nil {
		t.Fatalf("AccessHash miss: %v", err)
	}
	if ok {
		t.Fatal("AccessHash miss returned ok=true")
	}

	p := mtproto.PeerInfo{
		MTProtoID:  1234567890,
		BotAPIID:   mtproto.ToBotAPIID(1234567890),
		AccessHash: 424242,
		Title:      "My Channel",
		Username:   "",
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
	if err != nil || !ok {
		t.Fatalf("Get = (_, %v, %v), want a hit", ok, err)
	}
	if got.MTProtoID != p.MTProtoID || got.AccessHash != p.AccessHash || got.Title != p.Title {
		t.Errorf("Get = %+v, want the stored values", got)
	}
	if got.BotAPIID != -1001234567890 {
		t.Errorf("BotAPIID = %d, want -1001234567890", got.BotAPIID)
	}

	// Refresh overwrites in place.
	p.AccessHash = 434343
	p.Username = "chan"
	if err := c.Store(ctx, p); err != nil {
		t.Fatalf("Store refresh: %v", err)
	}
	got2, ok, err := c.Get(ctx, 1234567890)
	if err != nil || !ok {
		t.Fatalf("Get after refresh = (_, %v, %v)", ok, err)
	}
	if got2.AccessHash != 434343 || got2.Username != "chan" {
		t.Errorf("after refresh = %+v, want updated hash/username", got2)
	}
}
