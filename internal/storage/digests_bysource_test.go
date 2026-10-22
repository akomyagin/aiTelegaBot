// ListDigestItemsBySource (Stage 8 D-1, план §D — выборка для L3):
// по источнику / sourceID=0 → все строки / пустой результат / порядок id ASC.

package storage

import (
	"context"
	"testing"
)

func TestListDigestItemsBySource(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	digestID, err := st.InsertDigest(ctx, 500, 1, "body", 4, "")
	if err != nil {
		t.Fatalf("InsertDigest: %v", err)
	}
	items := []DigestItem{
		{SourceID: 7, DedupKey: "k1", Title: "A"},
		{SourceID: 8, DedupKey: "k2", Title: "B"},
		{SourceID: 7, DedupKey: "k3", Title: "C"},
		{DedupKey: "k4", Title: "D"}, // SourceID 0 → NULL
	}
	if err := st.MarkDelivered(ctx, digestID, items); err != nil {
		t.Fatalf("MarkDelivered: %v", err)
	}
	// Соседний дайджест с тем же источником не должен подмешиваться в выборку.
	otherID, err := st.InsertDigest(ctx, 500, 2, "body2", 1, "")
	if err != nil {
		t.Fatalf("InsertDigest other: %v", err)
	}
	if err := st.MarkDelivered(ctx, otherID, []DigestItem{{SourceID: 7, DedupKey: "k5", Title: "E"}}); err != nil {
		t.Fatalf("MarkDelivered other: %v", err)
	}

	// По источнику: только строки source_id=7 ЭТОГО дайджеста, id ASC.
	got, err := st.ListDigestItemsBySource(ctx, digestID, 7)
	if err != nil {
		t.Fatalf("BySource(7): %v", err)
	}
	if len(got) != 2 || got[0].Title != "A" || got[1].Title != "C" {
		t.Fatalf("BySource(7) = %+v, want [A C]", got)
	}
	if got[0].ID >= got[1].ID {
		t.Errorf("порядок не id ASC: %d, %d", got[0].ID, got[1].ID)
	}
	if got[0].SourceID != 7 || got[1].SourceID != 7 {
		t.Errorf("SourceID = %d, %d; want 7, 7", got[0].SourceID, got[1].SourceID)
	}

	// sourceID=0 → ВСЕ строки дайджеста (план D: L3:<did>:0 = общий список),
	// включая NULL-источник, id ASC.
	all, err := st.ListDigestItemsBySource(ctx, digestID, 0)
	if err != nil {
		t.Fatalf("BySource(0): %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("BySource(0) = %d строк, want 4", len(all))
	}
	for i, want := range []string{"A", "B", "C", "D"} {
		if all[i].Title != want {
			t.Errorf("all[%d] = %q, want %q (id ASC)", i, all[i].Title, want)
		}
	}

	// Неизвестный источник → пусто, НЕ ошибка (crafted id не роняет выборку).
	none, err := st.ListDigestItemsBySource(ctx, digestID, 999)
	if err != nil || len(none) != 0 {
		t.Errorf("BySource(999) = %d строк, err=%v; want пусто без ошибки", len(none), err)
	}
	// Неизвестный дайджест → пусто, НЕ ошибка.
	empty, err := st.ListDigestItemsBySource(ctx, 999999, 7)
	if err != nil || len(empty) != 0 {
		t.Errorf("BySource(digest 999999) = %d строк, err=%v; want пусто без ошибки", len(empty), err)
	}
}
