package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// TestMigrate_FreshDBReachesLatestVersion: a fresh Open runs all migrations
// (user_version=3 since Stage 8 B) and a repeated Open of the same file is
// idempotent. The v2→v3 upgrade path itself is covered in topics_test.go.
func TestMigrate_FreshDBReachesLatestVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v2.db")
	ctx := context.Background()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	var version int
	if err := st.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != 3 {
		t.Errorf("user_version = %d, want 3", version)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopening must not fail or double-apply.
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = st2.Close() }()
	if err := st2.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read user_version after reopen: %v", err)
	}
	if version != 3 {
		t.Errorf("user_version after reopen = %d, want 3", version)
	}
}

// TestSourcesKindRefUniqueIndex verifies migration v2 created the UNIQUE
// index and that duplicate (kind, ref) inserts are rejected.
func TestSourcesKindRefUniqueIndex(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	var name string
	err := st.db.QueryRowContext(ctx,
		"SELECT name FROM sqlite_master WHERE type='index' AND name='sources_kind_ref'").Scan(&name)
	if err == sql.ErrNoRows {
		t.Fatal("index sources_kind_ref does not exist")
	}
	if err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}

	if _, err := st.AddSource(ctx, "tg_public", "chan"); err != nil {
		t.Fatalf("first AddSource: %v", err)
	}
	if _, err := st.AddSource(ctx, "tg_public", "chan"); err == nil {
		t.Fatal("duplicate (kind, ref) AddSource succeeded; UNIQUE index missing or not enforced")
	}
}

// TestAddSourceIfNotExists: the first call creates, the second returns the
// same id with created=false; only one row exists.
func TestAddSourceIfNotExists(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	id1, created, reactivated, err := st.AddSourceIfNotExists(ctx, "tg_mtproto", "-1001234567890")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if !created || reactivated || id1 <= 0 {
		t.Fatalf("first call = (%d, %v, %v), want (id>0, true, false)", id1, created, reactivated)
	}

	id2, created2, reactivated2, err := st.AddSourceIfNotExists(ctx, "tg_mtproto", "-1001234567890")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if created2 || reactivated2 {
		t.Errorf("second call created=%v reactivated=%v, want (false, false)", created2, reactivated2)
	}
	if id2 != id1 {
		t.Errorf("second call id = %d, want the same %d", id2, id1)
	}

	rows, err := st.ListSources(ctx)
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected exactly 1 row, got %d", len(rows))
	}
	if !rows[0].Enabled {
		t.Errorf("row should be enabled")
	}

	// A different (kind, ref) still inserts normally.
	if _, created3, reactivated3, err := st.AddSourceIfNotExists(ctx, "tg_public", "chan"); err != nil || !created3 || reactivated3 {
		t.Errorf("different kind/ref: created=%v reactivated=%v err=%v, want (true, false, nil)", created3, reactivated3, err)
	}
}

// TestAddSourceIfNotExists_ReactivatesDisabled: re-add after DisableSource
// re-enables the SAME row (review H1); a further add of the enabled row
// returns both flags false and the same id.
func TestAddSourceIfNotExists_ReactivatesDisabled(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	id1, created, reactivated, err := st.AddSourceIfNotExists(ctx, "tg_botapi", "chan")
	if err != nil || !created || reactivated {
		t.Fatalf("add: (%d, %v, %v, %v), want (id, true, false, nil)", id1, created, reactivated, err)
	}

	if ok, err := st.DisableSource(ctx, id1); err != nil || !ok {
		t.Fatalf("DisableSource: ok=%v err=%v", ok, err)
	}

	id2, created2, reactivated2, err := st.AddSourceIfNotExists(ctx, "tg_botapi", "chan")
	if err != nil {
		t.Fatalf("re-add after disable: %v", err)
	}
	if created2 || !reactivated2 {
		t.Errorf("re-add after disable: created=%v reactivated=%v, want (false, true)", created2, reactivated2)
	}
	if id2 != id1 {
		t.Errorf("re-add id = %d, want the same %d", id2, id1)
	}
	rows, err := st.ListSources(ctx)
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected exactly 1 row, got %d", len(rows))
	}
	if !rows[0].Enabled {
		t.Errorf("row should be enabled after reactivation")
	}

	// Duplicate of the now-enabled row: both flags false, same id.
	id3, created3, reactivated3, err := st.AddSourceIfNotExists(ctx, "tg_botapi", "chan")
	if err != nil {
		t.Fatalf("add enabled duplicate: %v", err)
	}
	if created3 || reactivated3 || id3 != id1 {
		t.Errorf("enabled duplicate = (%d, %v, %v), want (%d, false, false)", id3, created3, reactivated3, id1)
	}
}

// TestPeerRoundtrip covers UpsertPeer/GetPeer: insert, refresh, unknown peer.
func TestPeerRoundtrip(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	_, ok, err := st.GetPeer(ctx, 1234567890)
	if err != nil {
		t.Fatalf("GetPeer unknown: %v", err)
	}
	if ok {
		t.Fatal("GetPeer on empty table returned ok=true")
	}

	if err := st.UpsertPeer(ctx, 1234567890, 424242, "My Channel", ""); err != nil {
		t.Fatalf("UpsertPeer insert: %v", err)
	}
	p, ok, err := st.GetPeer(ctx, 1234567890)
	if err != nil || !ok {
		t.Fatalf("GetPeer after insert: ok=%v err=%v", ok, err)
	}
	if p.MTProtoID != 1234567890 || p.AccessHash != 424242 || p.Title != "My Channel" || p.Username != "" {
		t.Errorf("peer = %+v, want the stored values", p)
	}
	if p.UpdatedAt == "" {
		t.Errorf("UpdatedAt should be set")
	}

	// Upsert refreshes in place (same PK, no second row).
	if err := st.UpsertPeer(ctx, 1234567890, 434343, "Renamed", "chan"); err != nil {
		t.Fatalf("UpsertPeer refresh: %v", err)
	}
	p2, ok, err := st.GetPeer(ctx, 1234567890)
	if err != nil || !ok {
		t.Fatalf("GetPeer after refresh: ok=%v err=%v", ok, err)
	}
	if p2.AccessHash != 434343 || p2.Title != "Renamed" || p2.Username != "chan" {
		t.Errorf("refreshed peer = %+v, want updated values", p2)
	}
}
