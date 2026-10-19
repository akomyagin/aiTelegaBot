package app

import (
	"net/http"
	"path/filepath"
	"testing"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
	"github.com/akomyagin/aiTelegaBot/internal/mtproto"
	"github.com/akomyagin/aiTelegaBot/internal/storage"
	"github.com/akomyagin/aiTelegaBot/internal/telegram"
)

// newTestMTClient builds an MTProto client without any network activity:
// construction only wires gotd internals; connecting happens on Run, which
// these tests never call.
func newTestMTClient(t *testing.T) *mtproto.Client {
	t.Helper()
	c, err := mtproto.NewClient(mtproto.MTProtoConfig{
		AppID:       1,
		AppHash:     "test-hash",
		SessionPath: filepath.Join(t.TempDir(), "test.session"),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func TestBuildDynamicSources_Kinds(t *testing.T) {
	mtClient := newTestMTClient(t)
	rows := []storage.Source{
		{ID: 1, Kind: "rss", Ref: "https://example.com/feed.xml", Enabled: true},
		{ID: 2, Kind: "tg_public", Ref: "chan", Enabled: true},
		{ID: 3, Kind: "tg_botapi", Ref: "managed", Enabled: true},
		{ID: 4, Kind: "tg_botapi", Ref: "-1001234567890", Enabled: true},  // private bot-admin chat
		{ID: 5, Kind: "tg_mtproto", Ref: "-1001234567890", Enabled: true}, // numeric -> PeerSource
		{ID: 6, Kind: "tg_mtproto", Ref: "@user", Enabled: true},          // username -> ChannelSource
		{ID: 7, Kind: "rss", Ref: "https://example.com/off", Enabled: false},
		{ID: 8, Kind: "unknown_kind", Ref: "x", Enabled: true},
	}

	out := buildDynamicSources(rows, http.DefaultClient, 20, &telegram.ChannelBuffer{}, mtClient, 20)

	if len(out) != 6 {
		t.Fatalf("got %d sources, want 6 (disabled and unknown skipped): %v", len(out), names(out))
	}
	if _, ok := out[0].(*feed.RSSSource); !ok {
		t.Errorf("rss row: got %T, want *feed.RSSSource", out[0])
	}
	if _, ok := out[1].(*telegram.PublicSource); !ok {
		t.Errorf("tg_public row: got %T, want *telegram.PublicSource", out[1])
	}
	ms, ok := out[2].(*telegram.ManagedSource)
	if !ok {
		t.Fatalf("tg_botapi row: got %T, want *telegram.ManagedSource", out[2])
	}
	if ms.Name() != "@managed" {
		t.Errorf("tg_botapi name = %q, want @managed", ms.Name())
	}
	if _, ok := out[3].(*telegram.ManagedSource); !ok {
		t.Errorf("tg_botapi numeric row: got %T, want *telegram.ManagedSource", out[3])
	} else if out[3].Name() != "-1001234567890" {
		t.Errorf("tg_botapi numeric name = %q, want the numeric ref", out[3].Name())
	}
	if _, ok := out[4].(*mtproto.PeerSource); !ok {
		t.Errorf("tg_mtproto numeric row: got %T, want *mtproto.PeerSource", out[4])
	}
	if _, ok := out[5].(*mtproto.ChannelSource); !ok {
		t.Errorf("tg_mtproto @username row: got %T, want *mtproto.ChannelSource", out[5])
	}
}

// TestBuildDynamicSources_NilMTClientSkipsMTProto: MTProto not configured
// (the VPS degradation path) must skip tg_mtproto rows, not crash.
func TestBuildDynamicSources_NilMTClientSkipsMTProto(t *testing.T) {
	rows := []storage.Source{
		{ID: 1, Kind: "tg_mtproto", Ref: "-1001234567890", Enabled: true},
		{ID: 2, Kind: "tg_public", Ref: "chan", Enabled: true},
	}
	out := buildDynamicSources(rows, http.DefaultClient, 20, &telegram.ChannelBuffer{}, nil, 20)
	if len(out) != 1 {
		t.Fatalf("got %d sources, want 1 (tg_mtproto skipped): %v", len(out), names(out))
	}
	if _, ok := out[0].(*telegram.PublicSource); !ok {
		t.Errorf("got %T, want *telegram.PublicSource", out[0])
	}
}

func TestManagedSourceNameKey(t *testing.T) {
	tests := []struct {
		ref      string
		wantName string
		wantKey  string
	}{
		{"chan", "@chan", "chan"},
		{"-1001234567890", "-1001234567890", "-1001234567890"},
	}
	for _, tt := range tests {
		name, key := managedSourceNameKey(tt.ref)
		if name != tt.wantName || key != tt.wantKey {
			t.Errorf("managedSourceNameKey(%q) = (%q, %q), want (%q, %q)",
				tt.ref, name, key, tt.wantName, tt.wantKey)
		}
	}
}

func names(srcs []feed.Source) []string {
	out := make([]string, 0, len(srcs))
	for _, s := range srcs {
		out = append(out, s.Name())
	}
	return out
}
