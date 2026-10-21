package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
)

// makeV3DB creates a database at user_version=3 (migrations v1–v3 applied
// verbatim from the shipped DDL) with one legacy digests row, to test the
// v3→v4 upgrade path (тест-кейс C1 №1).
func makeV3DB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer db.Close()
	for i := 0; i < 3; i++ {
		if _, err := db.Exec(migrations[i]); err != nil {
			t.Fatalf("apply migration %d: %v", i+1, err)
		}
	}
	if _, err := db.Exec("PRAGMA user_version = 3"); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	if _, err := db.Exec(
		"INSERT INTO digests(created_at, item_count, body, delivered) VALUES('2026-01-01T00:00:00Z', 2, 'legacy body', 1)"); err != nil {
		t.Fatalf("insert legacy digest: %v", err)
	}
}

func TestMigrate_FromV3ReachesLatest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v3.db")
	ctx := context.Background()
	makeV3DB(t, path)

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open over v3 db: %v", err)
	}

	var version int
	if err := st.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != 5 {
		t.Errorf("user_version = %d, want 5", version)
	}

	// digest_items table and its index exist.
	var name string
	if err := st.db.QueryRowContext(ctx,
		"SELECT name FROM sqlite_master WHERE type='table' AND name='digest_items'").Scan(&name); err != nil {
		t.Errorf("digest_items table missing: %v", err)
	}
	if err := st.db.QueryRowContext(ctx,
		"SELECT name FROM sqlite_master WHERE type='index' AND name='digest_items_digest'").Scan(&name); err != nil {
		t.Errorf("digest_items_digest index missing: %v", err)
	}

	// Legacy digests row: topic_id NULL, run_id 0.
	var legacyID int64
	var topicID sql.NullInt64
	var runID int64
	if err := st.db.QueryRowContext(ctx,
		"SELECT id, topic_id, run_id FROM digests WHERE body='legacy body'").Scan(&legacyID, &topicID, &runID); err != nil {
		t.Fatalf("select legacy digest: %v", err)
	}
	if topicID.Valid {
		t.Errorf("legacy topic_id = %d, want NULL", topicID.Int64)
	}
	if runID != 0 {
		t.Errorf("legacy run_id = %d, want 0", runID)
	}
	// Read back through GetDigest: COALESCE(topic_id,1) → 1, delivered preserved.
	d, ok, err := st.GetDigest(ctx, legacyID)
	if err != nil || !ok {
		t.Fatalf("GetDigest legacy: ok=%v err=%v", ok, err)
	}
	if d.TopicID != 1 || d.RunID != 0 || !d.Delivered {
		t.Errorf("legacy digest = %+v, want TopicID 1 (COALESCE), RunID 0, Delivered=true", d)
	}

	// Reopen: v4 is idempotent (no re-apply, no failure).
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = st2.Close() }()
	if err := st2.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read user_version after reopen: %v", err)
	}
	if version != 5 {
		t.Errorf("user_version after reopen = %d, want 5", version)
	}
}

func TestInsertDigestAndGet(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	id1, err := st.InsertDigest(ctx, 100, 5, "<b>body</b>", 3)
	if err != nil {
		t.Fatalf("InsertDigest 1: %v", err)
	}
	id2, err := st.InsertDigest(ctx, 100, 7, "body2", 1)
	if err != nil {
		t.Fatalf("InsertDigest 2: %v", err)
	}
	if id2 != id1+1 {
		t.Errorf("ids = %d, %d; want sequential", id1, id2)
	}

	d, ok, err := st.GetDigest(ctx, id1)
	if err != nil || !ok {
		t.Fatalf("GetDigest: ok=%v err=%v", ok, err)
	}
	if d.ID != id1 || d.RunID != 100 || d.TopicID != 5 || d.ItemCount != 3 || d.Body != "<b>body</b>" || d.Delivered {
		t.Errorf("digest = %+v, want the inserted values with delivered=0", d)
	}
	if _, err := time.Parse(time.RFC3339, d.CreatedAt); err != nil {
		t.Errorf("CreatedAt %q is not RFC3339: %v", d.CreatedAt, err)
	}
}

func TestGetDigest_NotFound(t *testing.T) {
	st := newTestStore(t)
	d, ok, err := st.GetDigest(context.Background(), 999999)
	if err != nil {
		t.Fatalf("GetDigest crafted id: err=%v, want nil (ok=false is NOT an error)", err)
	}
	if ok {
		t.Errorf("ok=true for unknown id, got %+v", d)
	}
}

func TestMarkDelivered(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	rssItem := feed.Item{
		Kind: "rss", Source: "HN", Title: "T1", URL: "https://a.example/1",
		Text: "text one", Published: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), SourceID: 7,
	}
	tgItem := feed.Item{Kind: "tg_botapi", Source: "@chan", Title: "T2", ID: "42", Text: "text two"}

	digestID, err := st.InsertDigest(ctx, 200, 1, "body", 2)
	if err != nil {
		t.Fatalf("InsertDigest: %v", err)
	}
	if err := st.MarkDelivered(ctx, digestID, []DigestItem{
		NewDigestItem(rssItem, "sum1", ""),
		NewDigestItem(tgItem, "", ""),
	}); err != nil {
		t.Fatalf("MarkDelivered: %v", err)
	}

	d, ok, err := st.GetDigest(ctx, digestID)
	if err != nil || !ok {
		t.Fatalf("GetDigest after MarkDelivered: ok=%v err=%v", ok, err)
	}
	if !d.Delivered {
		t.Error("delivered=0 after MarkDelivered, want 1")
	}

	items, err := st.ListDigestItems(ctx, digestID)
	if err != nil {
		t.Fatalf("ListDigestItems: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	if items[0].ID >= items[1].ID {
		t.Errorf("order not id ASC: %d, %d", items[0].ID, items[1].ID)
	}
	if items[0].DedupKey != rssItem.DedupKey() || items[0].Summary != "sum1" ||
		items[0].SourceID != 7 || items[0].Title != "T1" || items[0].URL != "https://a.example/1" ||
		items[0].PublishedAt != "2026-01-02T03:04:05Z" || items[0].Snippet != "text one" {
		t.Errorf("item0 = %+v", items[0])
	}
	if items[1].TgPeer != "chan" || items[1].TgMsgID != 42 || items[1].SourceID != 0 {
		t.Errorf("item1 = %+v, want TgPeer chan, TgMsgID 42, SourceID 0 (NULL)", items[1])
	}

	// seen_items carries SourceID (7) for the rss item and NULL for SourceID=0.
	var seenSrc sql.NullInt64
	if err := st.db.QueryRowContext(ctx,
		"SELECT source_id FROM seen_items WHERE dedup_key=?", rssItem.DedupKey()).Scan(&seenSrc); err != nil {
		t.Fatalf("seen_items rss: %v", err)
	}
	if !seenSrc.Valid || seenSrc.Int64 != 7 {
		t.Errorf("seen source_id = %v, want 7", seenSrc)
	}
	if err := st.db.QueryRowContext(ctx,
		"SELECT source_id FROM seen_items WHERE dedup_key=?", tgItem.DedupKey()).Scan(&seenSrc); err != nil {
		t.Fatalf("seen_items tg: %v", err)
	}
	if seenSrc.Valid {
		t.Errorf("seen source_id = %d, want NULL", seenSrc.Int64)
	}

	// A later FilterUnseen sees nothing — the items are seen now.
	fresh, err := st.FilterUnseen(ctx, []feed.Item{rssItem, tgItem})
	if err != nil {
		t.Fatalf("FilterUnseen: %v", err)
	}
	if len(fresh) != 0 {
		t.Errorf("fresh = %d, want 0", len(fresh))
	}
}

func TestMarkDelivered_Rollback(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	digestID, err := st.InsertDigest(ctx, 300, 1, "body", 1)
	if err != nil {
		t.Fatalf("InsertDigest: %v", err)
	}
	// Break the schema mid-flight: the digest_items insert must fail and the
	// whole transaction (delivered flag + seen_items) must roll back.
	// (План предлагал «пустой dedup_key», но '' не нарушает NOT NULL в SQLite —
	// поэтому роняем prepared-insert удалением таблицы.)
	if _, err := st.db.ExecContext(ctx, "DROP TABLE digest_items"); err != nil {
		t.Fatalf("drop digest_items: %v", err)
	}

	item := feed.Item{Kind: "rss", Title: "X", URL: "https://x.example"}
	if err := st.MarkDelivered(ctx, digestID, []DigestItem{NewDigestItem(item, "", "")}); err == nil {
		t.Fatal("MarkDelivered over a dropped table succeeded, want error")
	}

	d, ok, err := st.GetDigest(ctx, digestID)
	if err != nil || !ok {
		t.Fatalf("GetDigest: ok=%v err=%v", ok, err)
	}
	if d.Delivered {
		t.Error("delivered=1 after rollback, want 0")
	}
	fresh, err := st.FilterUnseen(ctx, []feed.Item{item})
	if err != nil {
		t.Fatalf("FilterUnseen: %v", err)
	}
	if len(fresh) != 1 {
		t.Errorf("item marked seen despite rollback: fresh=%d, want 1", len(fresh))
	}
}

func TestListRunDigests(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// Run 111: topics inserted out of order (5 before 1) — read back topic ASC.
	if _, err := st.InsertDigest(ctx, 111, 5, "b5", 1); err != nil {
		t.Fatalf("InsertDigest 111/5: %v", err)
	}
	if _, err := st.InsertDigest(ctx, 111, 1, "b1", 2); err != nil {
		t.Fatalf("InsertDigest 111/1: %v", err)
	}
	// Run 222 must not mix in.
	if _, err := st.InsertDigest(ctx, 222, 3, "b3", 1); err != nil {
		t.Fatalf("InsertDigest 222/3: %v", err)
	}
	// Legacy run_id=0 row.
	if _, err := st.InsertDigest(ctx, 0, 1, "legacy", 1); err != nil {
		t.Fatalf("InsertDigest 0/1: %v", err)
	}

	got, err := st.ListRunDigests(ctx, 111)
	if err != nil {
		t.Fatalf("ListRunDigests(111): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("run 111 = %d digests, want 2", len(got))
	}
	if got[0].TopicID != 1 || got[1].TopicID != 5 {
		t.Errorf("run 111 topic order = %d,%d; want 1,5", got[0].TopicID, got[1].TopicID)
	}

	got2, err := st.ListRunDigests(ctx, 222)
	if err != nil {
		t.Fatalf("ListRunDigests(222): %v", err)
	}
	if len(got2) != 1 || got2[0].TopicID != 3 {
		t.Errorf("run 222 = %+v, want exactly the topic-3 digest", got2)
	}

	empty, err := st.ListRunDigests(ctx, 0)
	if err != nil {
		t.Fatalf("ListRunDigests(0): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("runID=0 returned %d rows, want 0 («не прогон»)", len(empty))
	}
}

func TestNewDigestItem(t *testing.T) {
	pub := time.Date(2026, 3, 4, 5, 6, 7, 0, time.FixedZone("X", 3*3600))
	long := strings.Repeat("а", 250)
	huge := strings.Repeat("б", 8001)

	tests := []struct {
		name  string
		in    feed.Item
		sum   string
		class string
		want  DigestItem
	}{
		{
			name:  "rss: no tg fields, snippet cut to 200 runes, full_text kept, SourceID copied",
			in:    feed.Item{Kind: "rss", Source: "HN", Title: "T", URL: "https://u", Text: long, Published: pub, SourceID: 9},
			sum:   "s",
			class: "news",
			want: DigestItem{SourceID: 9, DedupKey: "https://u", Title: "T", URL: "https://u",
				PublishedAt: "2026-03-04T02:06:07Z", Snippet: strings.Repeat("а", 200), Summary: "s",
				FullText: long, Class: "news"},
		},
		{
			name:  "tg_botapi: peer without @, numeric msg id",
			in:    feed.Item{Kind: "tg_botapi", Source: "@chan", ID: "42", Title: "P"},
			class: "media",
			want: DigestItem{DedupKey: "tg:@chan:42", Title: "P", TgPeer: "chan", TgMsgID: 42,
				PublishedAt: "0001-01-01T00:00:00Z", Class: "media"},
		},
		{
			name: "tg_mtproto: -100X peer kept, unparseable id → 0",
			in:   feed.Item{Kind: "tg_mtproto", Source: "-100123", ID: "abc", Title: "M"},
			want: DigestItem{DedupKey: "tg:-100123:abc", Title: "M", TgPeer: "-100123", TgMsgID: 0,
				PublishedAt: "0001-01-01T00:00:00Z"},
		},
		{
			name: "full_text >8000 runes cut to 8000",
			in:   feed.Item{Kind: "rss", Title: "F", URL: "https://f", Text: huge},
			want: DigestItem{DedupKey: "https://f", Title: "F", URL: "https://f",
				PublishedAt: "0001-01-01T00:00:00Z", Snippet: strings.Repeat("б", 200),
				FullText: strings.Repeat("б", 8000)},
		},
		{
			name: "media: MediaFileID wins over MediaURL",
			in: feed.Item{Kind: "tg_botapi", Source: "@chan", ID: "7", Title: "Ph",
				MediaKind: "photo", MediaFileID: "AgACAgIAAxk", MediaURL: "https://img"},
			class: "media",
			want: DigestItem{DedupKey: "tg:@chan:7", Title: "Ph", TgPeer: "chan", TgMsgID: 7,
				PublishedAt: "0001-01-01T00:00:00Z", Class: "media", MediaKind: "photo", MediaRef: "AgACAgIAAxk"},
		},
		{
			name: "media: empty MediaFileID → MediaURL",
			in: feed.Item{Kind: "rss", Title: "Img", URL: "https://i",
				MediaKind: "photo", MediaURL: "https://i/pic.png"},
			want: DigestItem{DedupKey: "https://i", Title: "Img", URL: "https://i",
				PublishedAt: "0001-01-01T00:00:00Z", MediaKind: "photo", MediaRef: "https://i/pic.png"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NewDigestItem(tt.in, tt.sum, tt.class); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("NewDigestItem:\n got: %+v\nwant: %+v", got, tt.want)
			}
		})
	}
}

func TestDigests_RestartSurvival(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.sqlite")
	ctx := context.Background()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	digestID, err := st.InsertDigest(ctx, 400, 2, "body", 1)
	if err != nil {
		t.Fatalf("InsertDigest: %v", err)
	}
	item := feed.Item{Kind: "rss", Title: "R", URL: "https://r.example", Text: "snippet", SourceID: 3}
	if err := st.MarkDelivered(ctx, digestID, []DigestItem{NewDigestItem(item, "sum", "")}); err != nil {
		t.Fatalf("MarkDelivered: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// «Рестарт»: the same file reopened — everything must be on disk.
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = st2.Close() }()

	d, ok, err := st2.GetDigest(ctx, digestID)
	if err != nil || !ok {
		t.Fatalf("GetDigest after restart: ok=%v err=%v", ok, err)
	}
	if !d.Delivered || d.RunID != 400 || d.TopicID != 2 {
		t.Errorf("digest after restart = %+v", d)
	}
	items, err := st2.ListDigestItems(ctx, digestID)
	if err != nil {
		t.Fatalf("ListDigestItems after restart: %v", err)
	}
	if len(items) != 1 || items[0].DedupKey != "https://r.example" || items[0].SourceID != 3 || items[0].Summary != "sum" {
		t.Errorf("items after restart = %+v", items)
	}
}
