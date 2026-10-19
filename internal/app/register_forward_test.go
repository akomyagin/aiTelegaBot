package app

import (
	"context"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/akomyagin/aiTelegaBot/internal/storage"
	"github.com/akomyagin/aiTelegaBot/internal/telegram"
)

// newForwardTestStore opens a throwaway SQLite store (real migrations) for
// the forward-registration tests.
func newForwardTestStore(t *testing.T) *storage.SQLiteStore {
	t.Helper()
	st, err := storage.Open(filepath.Join(t.TempDir(), "forward.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// newTestRegistrar wires newForwardRegistrar + newSourceAdder over a real
// store with mtClient=nil: none of the branches under test may touch MTProto
// or the network.
//
// NOT covered here (deliberately):
//   - the tg_public branch (Username != "" && !BotIsAdmin) — its trial collect
//     hits the real t.me/s endpoint;
//   - addMTProtoSource with a live client — needs a network seam (tech debt,
//     recorded in the A1 review M3).
func newTestRegistrar(t *testing.T, st *storage.SQLiteStore) telegram.ForwardRegistrar {
	t.Helper()
	return newForwardRegistrar(st, nil, newSourceAdder(st, nil, http.DefaultClient, 20))
}

// TestRegisterForward_BotAdmin covers the tg_botapi branch: creation, the
// informational duplicate reply, HTML-escaping of the title, and reactivation
// after /removesource (review H1).
func TestRegisterForward_BotAdmin(t *testing.T) {
	st := newForwardTestStore(t)
	reg := newTestRegistrar(t, st)
	ctx := context.Background()

	origin := telegram.ForwardOrigin{
		BotIsAdmin:   true,
		BotAPIChatID: -1001111111111,
		Username:     "mychan",
		Title:        "My <Chan> & Co",
	}

	// (a) first forward: row created, reply mentions the id.
	reply := reg(ctx, origin)
	if !strings.Contains(reply, "✅") || !strings.Contains(reply, "добавлен") {
		t.Fatalf("first reply = %q, want a ✅ «добавлен» text", reply)
	}
	if strings.Contains(reply, "<Chan>") || !strings.Contains(reply, "&lt;Chan&gt;") {
		t.Errorf("title must be HTML-escaped in the reply: %q", reply)
	}
	rows, err := st.ListSources(ctx)
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	if len(rows) != 1 || rows[0].Kind != "tg_botapi" || rows[0].Ref != "mychan" || !rows[0].Enabled {
		t.Fatalf("rows = %+v, want one enabled tg_botapi/mychan row", rows)
	}
	if !strings.Contains(reply, "#"+strconv.FormatInt(rows[0].ID, 10)) {
		t.Errorf("reply %q should mention the source id #%d", reply, rows[0].ID)
	}
	id := rows[0].ID

	// (г) duplicate forward: informational, NOT an error tone.
	reply2 := reg(ctx, origin)
	if !strings.Contains(reply2, "ℹ️") || !strings.Contains(reply2, "уже добавлен") {
		t.Fatalf("duplicate reply = %q, want «ℹ️ … уже добавлен»", reply2)
	}
	if strings.Contains(reply2, "❌") {
		t.Errorf("duplicate reply must not use ❌: %q", reply2)
	}
	if rows2, _ := st.ListSources(ctx); len(rows2) != 1 {
		t.Fatalf("duplicate created a second row: %+v", rows2)
	}

	// (д) /removesource then forward again: reactivation (review H1).
	if ok, err := st.DisableSource(ctx, id); err != nil || !ok {
		t.Fatalf("DisableSource: ok=%v err=%v", ok, err)
	}
	reply3 := reg(ctx, origin)
	if !strings.Contains(reply3, "✅") || !strings.Contains(reply3, "снова включён") {
		t.Fatalf("reactivation reply = %q, want «✅ … снова включён»", reply3)
	}
	rows3, err := st.ListSources(ctx)
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	if len(rows3) != 1 || rows3[0].ID != id || !rows3[0].Enabled {
		t.Fatalf("rows = %+v, want the same row #%d enabled again", rows3, id)
	}
}

// TestRegisterForward_BotAdminPrivateChat: (б) BotIsAdmin without a username
// registers the numeric "-100X" ref (per-key buffer key, plan §4.5).
func TestRegisterForward_BotAdminPrivateChat(t *testing.T) {
	st := newForwardTestStore(t)
	reg := newTestRegistrar(t, st)
	ctx := context.Background()

	reply := reg(ctx, telegram.ForwardOrigin{
		BotIsAdmin:   true,
		BotAPIChatID: -1001234567890,
		Title:        "Private chat",
	})
	if !strings.Contains(reply, "✅") || !strings.Contains(reply, "добавлен") {
		t.Fatalf("reply = %q, want a ✅ «добавлен» text", reply)
	}
	rows, err := st.ListSources(ctx)
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	if len(rows) != 1 || rows[0].Kind != "tg_botapi" || rows[0].Ref != "-1001234567890" {
		t.Fatalf("rows = %+v, want one tg_botapi/-1001234567890 row", rows)
	}
}

// TestRegisterForward_NilMTClientDegrades: (в) a private-chat forward without
// MTProto configured answers with the degradation text and creates nothing.
func TestRegisterForward_NilMTClientDegrades(t *testing.T) {
	st := newForwardTestStore(t)
	reg := newTestRegistrar(t, st)
	ctx := context.Background()

	reply := reg(ctx, telegram.ForwardOrigin{
		BotAPIChatID: -1002222222222,
		Title:        "Private channel",
	})
	if !strings.Contains(reply, "MTPROTO_APP_ID") || !strings.Contains(reply, "MTPROTO_APP_HASH") {
		t.Fatalf("reply = %q, want the MTPROTO_APP_ID/HASH degradation text", reply)
	}
	if strings.Contains(reply, "❌") {
		t.Errorf("degradation is a first-class path, not an error: %q", reply)
	}
	rows, err := st.ListSources(ctx)
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("no source must be created, got %+v", rows)
	}
}
