package storage

import (
	"context"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestAddSource_IncreasingIDs(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	id1, err := st.AddSource(ctx, "rss", "https://example.com/feed.xml")
	if err != nil {
		t.Fatalf("AddSource #1: %v", err)
	}
	id2, err := st.AddSource(ctx, "tg_public", "golang_news")
	if err != nil {
		t.Fatalf("AddSource #2: %v", err)
	}
	if id2 <= id1 {
		t.Fatalf("expected increasing ids, got id1=%d id2=%d", id1, id2)
	}

	rows, err := st.ListSources(ctx)
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	if rows[0].ID != id1 || rows[0].Kind != "rss" || rows[0].Ref != "https://example.com/feed.xml" {
		t.Errorf("row 0 mismatch: %+v", rows[0])
	}
	if !rows[0].Enabled {
		t.Errorf("row 0 should be enabled by default")
	}
	if rows[0].AddedAt == "" {
		t.Errorf("row 0 AddedAt should not be empty")
	}
	if rows[1].ID != id2 || rows[1].Kind != "tg_public" || rows[1].Ref != "golang_news" {
		t.Errorf("row 1 mismatch: %+v", rows[1])
	}
}

func TestListSources_EmptyTable(t *testing.T) {
	st := newTestStore(t)
	rows, err := st.ListSources(context.Background())
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected 0 rows on empty table, got %d", len(rows))
	}
}

func TestListSources_OrderedByID(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	var ids []int64
	for _, ref := range []string{"a", "b", "c"} {
		id, err := st.AddSource(ctx, "rss", ref)
		if err != nil {
			t.Fatalf("AddSource(%s): %v", ref, err)
		}
		ids = append(ids, id)
	}

	rows, err := st.ListSources(ctx)
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(rows))
	}
	for i, id := range ids {
		if rows[i].ID != id {
			t.Errorf("row %d: id = %d, want %d (order not ascending)", i, rows[i].ID, id)
		}
	}
}

func TestDisableSource_ExistingID(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	id, err := st.AddSource(ctx, "rss", "https://example.com/feed.xml")
	if err != nil {
		t.Fatalf("AddSource: %v", err)
	}

	found, err := st.DisableSource(ctx, id)
	if err != nil {
		t.Fatalf("DisableSource: %v", err)
	}
	if !found {
		t.Fatalf("DisableSource(%d) = found=false, want true", id)
	}

	rows, err := st.ListSources(ctx)
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	if len(rows) != 1 || rows[0].Enabled {
		t.Fatalf("expected disabled row after DisableSource, got %+v", rows)
	}
}

func TestDisableSource_NonexistentID(t *testing.T) {
	st := newTestStore(t)
	found, err := st.DisableSource(context.Background(), 9999)
	if err != nil {
		t.Fatalf("DisableSource(nonexistent): unexpected error %v", err)
	}
	if found {
		t.Fatalf("DisableSource(nonexistent) = found=true, want false")
	}
}

func TestDisableSource_Idempotent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	id, err := st.AddSource(ctx, "rss", "https://example.com/feed.xml")
	if err != nil {
		t.Fatalf("AddSource: %v", err)
	}

	// First disable: row transitions enabled -> disabled, found=true.
	found1, err := st.DisableSource(ctx, id)
	if err != nil || !found1 {
		t.Fatalf("first DisableSource: found=%v err=%v, want true,nil", found1, err)
	}
	// Second disable: row still exists (UPDATE matches it again), found=true.
	found2, err := st.DisableSource(ctx, id)
	if err != nil || !found2 {
		t.Fatalf("second DisableSource: found=%v err=%v, want true,nil", found2, err)
	}

	rows, err := st.ListSources(ctx)
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	if len(rows) != 1 || rows[0].Enabled {
		t.Fatalf("expected still-disabled row, got %+v", rows)
	}
}
