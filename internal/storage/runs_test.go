// Тесты выборок Этапа 9D-1 (план 9D тест-кейс 3): GetDigestItem (roundtrip;
// crafted id → ok=false БЕЗ ошибки — GetDigest-паттерн) и runs.summary
// (UpsertRunSummary/GetRunSummary roundtrip; miss → ("", false, nil);
// legacy runID=0). Temp SQLite, реальная миграция v5.

package storage

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
)

func openRunsStore(t *testing.T) *SQLiteStore {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestGetDigestItem_RoundtripAndMiss(t *testing.T) {
	ctx := context.Background()
	s := openRunsStore(t)

	did, err := s.InsertDigest(ctx, 1700000000, 1, "<b>body</b>", 1, "")
	if err != nil {
		t.Fatalf("InsertDigest: %v", err)
	}
	it := NewDigestItem(feed.Item{
		Kind: "rss", Source: "src", ID: "https://x/1",
		Title: "T", URL: "https://x/1", Text: "full text",
	}, "summary", "news")
	it.SourceID = 9
	it.MediaKind = "photo"
	it.MediaRef = "ref"
	if err := s.MarkDelivered(ctx, did, []DigestItem{it}); err != nil {
		t.Fatalf("MarkDelivered: %v", err)
	}
	listed, err := s.ListDigestItems(ctx, did)
	if err != nil || len(listed) != 1 {
		t.Fatalf("ListDigestItems: %v, %d rows", err, len(listed))
	}

	got, ok, err := s.GetDigestItem(ctx, listed[0].ID)
	if err != nil || !ok {
		t.Fatalf("GetDigestItem: ok=%v err=%v", ok, err)
	}
	if got.DigestID != did || got.ID != listed[0].ID || got.SourceID != 9 ||
		got.Title != "T" || got.URL != "https://x/1" || got.Summary != "summary" ||
		got.FullText != "full text" || got.Class != "news" ||
		got.MediaKind != "photo" || got.MediaRef != "ref" {
		t.Errorf("GetDigestItem = %+v", got)
	}
	// Контракт поля: List* не заполняют DigestID (только GetDigestItem).
	if listed[0].DigestID != 0 {
		t.Errorf("ListDigestItems DigestID = %d, want 0", listed[0].DigestID)
	}
	// Crafted/устаревший id → ok=false, НЕ ошибка (ack диспетчера, не паника).
	for _, bad := range []int64{listed[0].ID + 1000, 0, -1} {
		if _, ok, err := s.GetDigestItem(ctx, bad); ok || err != nil {
			t.Errorf("GetDigestItem(%d): ok=%v err=%v, want false/nil", bad, ok, err)
		}
	}
}

func TestRunSummary_RoundtripMissLegacy(t *testing.T) {
	ctx := context.Background()
	s := openRunsStore(t)
	const runID = int64(1700000001)

	// miss → ("", false, nil)
	if sum, ok, err := s.GetRunSummary(ctx, runID); sum != "" || ok || err != nil {
		t.Fatalf("miss: %q %v %v", sum, ok, err)
	}
	// roundtrip: summary хранится байт-в-байт (HTML-спецсимволы не трогаются —
	// экранирование делает RenderTOC, не storage).
	want := "Общее саммари & <b>выпуск</b>"
	if err := s.UpsertRunSummary(ctx, runID, want); err != nil {
		t.Fatalf("UpsertRunSummary: %v", err)
	}
	if sum, ok, err := s.GetRunSummary(ctx, runID); err != nil || !ok || sum != want {
		t.Fatalf("roundtrip: %q %v %v", sum, ok, err)
	}
	// Повторный upsert того же run_id — перезапись, не дубль.
	if err := s.UpsertRunSummary(ctx, runID, "второе"); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if sum, ok, _ := s.GetRunSummary(ctx, runID); !ok || sum != "второе" {
		t.Errorf("re-upsert: %q %v, want «второе»", sum, ok)
	}
	// Пустое summary — валидная строка (сбой D деградирует, строка есть).
	if err := s.UpsertRunSummary(ctx, runID, ""); err != nil {
		t.Fatalf("upsert empty: %v", err)
	}
	if sum, ok, _ := s.GetRunSummary(ctx, runID); !ok || sum != "" {
		t.Errorf("empty: %q %v, want (\"\", true)", sum, ok)
	}
	// Legacy runID=0: get — miss без ошибки; upsert → error (0 — сентинел
	// «не прогон», строки runs для него быть не должно).
	if sum, ok, err := s.GetRunSummary(ctx, 0); sum != "" || ok || err != nil {
		t.Errorf("runID=0 get: %q %v %v", sum, ok, err)
	}
	if err := s.UpsertRunSummary(ctx, 0, "x"); err == nil {
		t.Error("UpsertRunSummary(0) — want error")
	}
	if err := s.UpsertRunSummary(ctx, -5, "x"); err == nil {
		t.Error("UpsertRunSummary(-5) — want error")
	}
}
