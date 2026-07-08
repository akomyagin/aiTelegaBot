package storage_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
	"github.com/akomyagin/aiTelegaBot/internal/storage"
)

func userVersion(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open for pragma: %v", err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	return v
}

func TestSQLiteStore(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	store, err := storage.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// 1. Open creates schema at version 1.
	if v := userVersion(t, path); v != 1 {
		t.Fatalf("user_version = %d, want 1", v)
	}

	items := []feed.Item{
		{URL: "https://a.test/1", Title: "A"},
		{URL: "https://a.test/2", Title: "B"},
		{Kind: "hn", Title: "no-url"},
	}

	// 2. MarkSeen the items.
	if err := store.MarkSeen(ctx, items); err != nil {
		t.Fatalf("MarkSeen: %v", err)
	}

	// 3. FilterUnseen with same items → empty.
	got, err := store.FilterUnseen(ctx, items)
	if err != nil {
		t.Fatalf("FilterUnseen (seen): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want 0 unseen, got %d", len(got))
	}

	// 4. FilterUnseen with new items → all returned.
	fresh := []feed.Item{
		{URL: "https://a.test/3", Title: "C"},
		{URL: "https://a.test/4", Title: "D"},
	}
	got, err = store.FilterUnseen(ctx, fresh)
	if err != nil {
		t.Fatalf("FilterUnseen (fresh): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 unseen, got %d", len(got))
	}

	// mixed: one seen, one new.
	mixed := []feed.Item{items[0], fresh[0]}
	got, err = store.FilterUnseen(ctx, mixed)
	if err != nil {
		t.Fatalf("FilterUnseen (mixed): %v", err)
	}
	if len(got) != 1 || got[0].URL != fresh[0].URL {
		t.Fatalf("mixed filter wrong: %+v", got)
	}

	// 5. SaveDigest.
	if err := store.SaveDigest(ctx, "body text", 3); err != nil {
		t.Fatalf("SaveDigest: %v", err)
	}

	// 6. GetMeta/SetMeta round-trip.
	if _, ok, err := store.GetMeta(ctx, "last_run"); err != nil || ok {
		t.Fatalf("GetMeta absent: ok=%v err=%v", ok, err)
	}
	if err := store.SetMeta(ctx, "last_run", "2024-01-01"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	v, ok, err := store.GetMeta(ctx, "last_run")
	if err != nil || !ok || v != "2024-01-01" {
		t.Fatalf("GetMeta = (%q,%v,%v)", v, ok, err)
	}
	// upsert overwrites.
	if err := store.SetMeta(ctx, "last_run", "2024-02-02"); err != nil {
		t.Fatalf("SetMeta update: %v", err)
	}
	if v, _, _ := store.GetMeta(ctx, "last_run"); v != "2024-02-02" {
		t.Fatalf("SetMeta upsert failed: %q", v)
	}

	// empty FilterUnseen returns empty without a query.
	if got, err := store.FilterUnseen(ctx, nil); err != nil || got != nil {
		t.Fatalf("FilterUnseen(nil) = (%v,%v)", got, err)
	}

	// 7. Reopen same path → migration idempotent, data intact.
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	store2, err := storage.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = store2.Close() })
	if v := userVersion(t, path); v != 1 {
		t.Fatalf("after reopen user_version = %d, want 1", v)
	}
	if got, _ := store2.FilterUnseen(ctx, []feed.Item{items[0]}); len(got) != 0 {
		t.Fatalf("seen data lost after reopen")
	}
}
