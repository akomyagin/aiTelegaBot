package storage

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
)

// TestUpsertMessageState_Roundtrip: батч-insert → чтение байт-в-байт →
// update того же ключа одной транзакцией → повторное чтение (тест-кейс 2
// этапа 9A: insert/update roundtrip; статусы/классы сохраняются байт-в-байт).
func TestUpsertMessageState_Roundtrip(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	in := []MessageState{
		{
			DedupKey: "tg:@chan:1", SourceID: 7, TopicID: 2,
			Class: "news", ClusterID: "500:1", IsPrimary: true,
			Summary: "Короткое саммари — 1–2 предложения.", MediaKind: "photo",
			MediaRef: "AgACAgIAAxk", Status: "done",
		},
		{
			DedupKey: "https://a.example/1", SourceID: 0, TopicID: 0,
			Class: "ad", IsPrimary: false, Summary: "", Status: "dropped_ad",
		},
	}
	if err := st.UpsertMessageState(ctx, in); err != nil {
		t.Fatalf("UpsertMessageState insert: %v", err)
	}

	got, err := st.GetMessageStates(ctx, []string{"tg:@chan:1", "https://a.example/1"})
	if err != nil {
		t.Fatalf("GetMessageStates: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d states, want 2", len(got))
	}
	for i, want := range in {
		g := got[want.DedupKey]
		if g.DedupKey != want.DedupKey || g.SourceID != want.SourceID || g.TopicID != want.TopicID ||
			g.Class != want.Class || g.ClusterID != want.ClusterID || g.IsPrimary != want.IsPrimary ||
			g.Summary != want.Summary || g.MediaKind != want.MediaKind || g.MediaRef != want.MediaRef ||
			g.Status != want.Status {
			t.Errorf("state %d =\n got %+v\nwant %+v", i, g, want)
		}
		if g.UpdatedAt == "" {
			t.Errorf("state %d: UpdatedAt not stamped", i)
		}
	}

	// Update того же ключа: статус/класс/саммари меняются, дубль строки не
	// появляется (PK = dedup_key).
	upd := []MessageState{{
		DedupKey: "tg:@chan:1", SourceID: 7, TopicID: 2,
		Class: "article", ClusterID: "", IsPrimary: true,
		Summary: "Обновлённое саммари", MediaKind: "", MediaRef: "", Status: "done",
	}}
	if err := st.UpsertMessageState(ctx, upd); err != nil {
		t.Fatalf("UpsertMessageState update: %v", err)
	}
	got2, err := st.GetMessageStates(ctx, []string{"tg:@chan:1"})
	if err != nil {
		t.Fatalf("GetMessageStates after update: %v", err)
	}
	if len(got2) != 1 {
		t.Fatalf("after update got %d states, want 1 (upsert, not insert)", len(got2))
	}
	g2 := got2["tg:@chan:1"]
	if g2.Class != "article" || g2.Summary != "Обновлённое саммари" || g2.ClusterID != "" ||
		g2.MediaKind != "" || g2.MediaRef != "" {
		t.Errorf("after update = %+v, want class=article and cleared cluster/media", g2)
	}

	// Байт-в-байт: нестандартные строки (кавычки, эмодзи, CRLF) не искажаются.
	exact := MessageState{
		DedupKey: `weird:"key'<>` + "\n", Class: "opinion",
		Summary: "«кавычки» 😀\r\nперенос", Status: "done", IsPrimary: true,
	}
	if err := st.UpsertMessageState(ctx, []MessageState{exact}); err != nil {
		t.Fatalf("UpsertMessageState exact: %v", err)
	}
	gotExact, err := st.GetMessageStates(ctx, []string{exact.DedupKey})
	if err != nil {
		t.Fatalf("GetMessageStates exact: %v", err)
	}
	gx := gotExact[exact.DedupKey]
	if gx.Class != exact.Class || gx.Summary != exact.Summary || gx.Status != exact.Status {
		t.Errorf("byte-exact roundtrip failed:\n got %+v\nwant %+v", gx, exact)
	}
}

// TestGetMessageStates_BatchMissingKeys: батч из 3 ключей — 2 найдено,
// 1 отсутствует в map (отсутствие НЕ ошибка).
func TestGetMessageStates_BatchMissingKeys(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if err := st.UpsertMessageState(ctx, []MessageState{
		{DedupKey: "k1", Class: "news", Status: "done", IsPrimary: true},
		{DedupKey: "k2", Class: "course", Status: "done", IsPrimary: true},
	}); err != nil {
		t.Fatalf("UpsertMessageState: %v", err)
	}

	got, err := st.GetMessageStates(ctx, []string{"k1", "k2", "k-missing"})
	if err != nil {
		t.Fatalf("GetMessageStates: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (missing key absent, not zero-valued)", len(got))
	}
	if _, ok := got["k-missing"]; ok {
		t.Error("k-missing present in the map, want absent")
	}
	if got["k1"].Class != "news" || got["k2"].Class != "course" {
		t.Errorf("classes = %q/%q, want news/course", got["k1"].Class, got["k2"].Class)
	}

	// Пустой список ключей — пустая map без запроса.
	empty, err := st.GetMessageStates(ctx, nil)
	if err != nil {
		t.Fatalf("GetMessageStates(nil): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("empty result = %d, want 0", len(empty))
	}
}

// TestGetMessageStates_Chunking: >500 ключей читаются несколькими запросами
// (чанки ≤500) без потерь.
func TestGetMessageStates_Chunking(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	const n = 600
	states := make([]MessageState, 0, n)
	keys := make([]string, 0, n)
	for i := 0; i < n; i++ {
		k := fmt.Sprintf("key-%04d", i)
		keys = append(keys, k)
		states = append(states, MessageState{DedupKey: k, Class: "other", Status: "done", IsPrimary: true})
	}
	if err := st.UpsertMessageState(ctx, states); err != nil {
		t.Fatalf("UpsertMessageState(%d): %v", n, err)
	}
	got, err := st.GetMessageStates(ctx, keys)
	if err != nil {
		t.Fatalf("GetMessageStates(%d): %v", n, err)
	}
	if len(got) != n {
		t.Errorf("len = %d, want %d (chunked reads lost keys)", len(got), n)
	}
}

// TestMarkSeenBatch: батч-вставка в seen_items одной транзакцией →
// последующий FilterUnseen пуст; повторный вызов идемпотентен.
func TestMarkSeenBatch(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	items := []feed.Item{
		{Kind: "rss", Title: "A", URL: "https://a.example"},
		{Kind: "tg_botapi", Source: "@chan", ID: "10", Title: "B"},
	}
	keys := make([]string, 0, len(items))
	for _, it := range items {
		keys = append(keys, it.DedupKey())
	}

	if err := st.MarkSeenBatch(ctx, keys); err != nil {
		t.Fatalf("MarkSeenBatch: %v", err)
	}

	fresh, err := st.FilterUnseen(ctx, items)
	if err != nil {
		t.Fatalf("FilterUnseen: %v", err)
	}
	if len(fresh) != 0 {
		t.Errorf("fresh = %d, want 0 after MarkSeenBatch", len(fresh))
	}

	// INSERT OR IGNORE: повтор не ошибка и не дубль.
	if err := st.MarkSeenBatch(ctx, keys); err != nil {
		t.Fatalf("MarkSeenBatch repeat: %v", err)
	}
	var count int
	placeholders := strings.Repeat("?,", len(keys)-1) + "?"
	args := make([]any, len(keys))
	for i, k := range keys {
		args[i] = k
	}
	if err := st.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM seen_items WHERE dedup_key IN ("+placeholders+")", args...).Scan(&count); err != nil {
		t.Fatalf("count seen_items: %v", err)
	}
	if count != len(keys) {
		t.Errorf("seen_items rows = %d, want %d", count, len(keys))
	}

	// Пустой батч — no-op без ошибки.
	if err := st.MarkSeenBatch(ctx, nil); err != nil {
		t.Errorf("MarkSeenBatch(nil) = %v, want nil", err)
	}
}

// Compile-time assert: *SQLiteStore удовлетворяет Store (включая новые методы
// message_state, этап 9A).
var _ Store = (*SQLiteStore)(nil)
