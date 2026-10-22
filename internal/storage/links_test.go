package storage

import (
	"context"
	"fmt"
	"testing"
)

// TestUpsertLinkCache_Roundtrip: батч-insert → чтение байт-в-байт →
// повторный upsert того же URL = update, не дубль (PK url); fetched_at
// штампуется внутри (тест-кейс 4 этапа 9B).
func TestUpsertLinkCache_Roundtrip(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	in := []LinkCacheRow{
		{URL: "https://a.example/1", Kind: "article", Title: "Заголовок", Summary: "Саммари статьи."},
		{URL: "https://b.example/2", Kind: "course", Title: "", Summary: ""},
	}
	if err := st.UpsertLinkCache(ctx, in); err != nil {
		t.Fatalf("UpsertLinkCache insert: %v", err)
	}

	got, err := st.GetLinkCache(ctx, []string{in[0].URL, in[1].URL})
	if err != nil {
		t.Fatalf("GetLinkCache: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2", len(got))
	}
	for i, want := range in {
		g := got[want.URL]
		if g.URL != want.URL || g.Kind != want.Kind || g.Title != want.Title || g.Summary != want.Summary {
			t.Errorf("row %d =\n got %+v\nwant %+v", i, g, want)
		}
		if g.FetchedAt == "" {
			t.Errorf("row %d: FetchedAt not stamped", i)
		}
	}

	// Повторный upsert того же URL — update (kind/title/summary), не дубль.
	upd := []LinkCacheRow{{URL: in[0].URL, Kind: "unavailable", Title: "T2", Summary: "S2"}}
	if err := st.UpsertLinkCache(ctx, upd); err != nil {
		t.Fatalf("UpsertLinkCache update: %v", err)
	}
	got2, err := st.GetLinkCache(ctx, []string{in[0].URL})
	if err != nil {
		t.Fatalf("GetLinkCache after update: %v", err)
	}
	if len(got2) != 1 {
		t.Fatalf("after update got %d rows, want 1", len(got2))
	}
	g2 := got2[in[0].URL]
	if g2.Kind != "unavailable" || g2.Title != "T2" || g2.Summary != "S2" {
		t.Errorf("after update = %+v, want kind=unavailable title=T2 summary=S2", g2)
	}
	var count int
	if err := st.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM link_cache").Scan(&count); err != nil {
		t.Fatalf("count link_cache: %v", err)
	}
	if count != 2 {
		t.Errorf("link_cache rows = %d, want 2 (upsert, not insert)", count)
	}

	// Байт-в-байт: кавычки/эмодзи/CRLF не искажаются.
	exact := LinkCacheRow{
		URL:     `https://c.example/"weird"?x=1&y=2`,
		Kind:    "other",
		Title:   "«кавычки» 😀\r\nперенос",
		Summary: "саммари с 'одинарными' и \"двойными\" 😀",
	}
	if err := st.UpsertLinkCache(ctx, []LinkCacheRow{exact}); err != nil {
		t.Fatalf("UpsertLinkCache exact: %v", err)
	}
	gotExact, err := st.GetLinkCache(ctx, []string{exact.URL})
	if err != nil {
		t.Fatalf("GetLinkCache exact: %v", err)
	}
	gx := gotExact[exact.URL]
	if gx.URL != exact.URL || gx.Kind != exact.Kind || gx.Title != exact.Title || gx.Summary != exact.Summary {
		t.Errorf("byte-exact roundtrip failed:\n got %+v\nwant %+v", gx, exact)
	}

	// Пустой батч — no-op без ошибки.
	if err := st.UpsertLinkCache(ctx, nil); err != nil {
		t.Errorf("UpsertLinkCache(nil) = %v, want nil", err)
	}
}

// TestGetLinkCache_BatchMissing: батч из 3 URL — 2 найдено, 1 отсутствует в
// map (отсутствие НЕ ошибка); nil-вход → пустая map.
func TestGetLinkCache_BatchMissing(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if err := st.UpsertLinkCache(ctx, []LinkCacheRow{
		{URL: "https://a.example/1", Kind: "article"},
		{URL: "https://b.example/2", Kind: "ad"},
	}); err != nil {
		t.Fatalf("UpsertLinkCache: %v", err)
	}

	got, err := st.GetLinkCache(ctx, []string{"https://a.example/1", "https://b.example/2", "https://missing.example"})
	if err != nil {
		t.Fatalf("GetLinkCache: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (missing URL absent, not zero-valued)", len(got))
	}
	if _, ok := got["https://missing.example"]; ok {
		t.Error("missing URL present in the map, want absent")
	}
	if got["https://a.example/1"].Kind != "article" || got["https://b.example/2"].Kind != "ad" {
		t.Errorf("kinds = %q/%q, want article/ad", got["https://a.example/1"].Kind, got["https://b.example/2"].Kind)
	}

	empty, err := st.GetLinkCache(ctx, nil)
	if err != nil {
		t.Fatalf("GetLinkCache(nil): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("empty result = %d, want 0", len(empty))
	}
}

// TestGetLinkCache_Chunking: >500 URL читаются несколькими запросами
// (чанки ≤500) без потерь.
func TestGetLinkCache_Chunking(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	const n = 600
	rows := make([]LinkCacheRow, 0, n)
	urls := make([]string, 0, n)
	for i := 0; i < n; i++ {
		u := fmt.Sprintf("https://x.example/%04d", i)
		urls = append(urls, u)
		rows = append(rows, LinkCacheRow{URL: u, Kind: "other"})
	}
	if err := st.UpsertLinkCache(ctx, rows); err != nil {
		t.Fatalf("UpsertLinkCache(%d): %v", n, err)
	}
	got, err := st.GetLinkCache(ctx, urls)
	if err != nil {
		t.Fatalf("GetLinkCache(%d): %v", n, err)
	}
	if len(got) != n {
		t.Errorf("len = %d, want %d (chunked reads lost URLs)", len(got), n)
	}
}

// TestItemLinks_UpsertListOrder: UpsertItemLinks/ListItemLinks — порядок
// ord ASC; повторная вставка того же (dedup_key,url) → update не дубль;
// roundtrip всех полей байт-в-байт; неизвестный ключ → пустой результат.
func TestItemLinks_UpsertListOrder(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// Вставка вне порядка ord — чтение всегда ord ASC.
	links := []ItemLink{
		{DedupKey: "k1", URL: "https://c.example", Kind: "other", Ord: 3},
		{DedupKey: "k1", URL: "https://a.example", Kind: "article", Ord: 1},
		{DedupKey: "k1", URL: "https://b.example", Kind: "ad", Ord: 2},
		{DedupKey: "k2", URL: "https://x.example", Kind: "course", Ord: 1},
	}
	if err := st.UpsertItemLinks(ctx, links); err != nil {
		t.Fatalf("UpsertItemLinks: %v", err)
	}

	got, err := st.ListItemLinks(ctx, "k1")
	if err != nil {
		t.Fatalf("ListItemLinks(k1): %v", err)
	}
	want := []ItemLink{
		{DedupKey: "k1", URL: "https://a.example", Kind: "article", Ord: 1},
		{DedupKey: "k1", URL: "https://b.example", Kind: "ad", Ord: 2},
		{DedupKey: "k1", URL: "https://c.example", Kind: "other", Ord: 3},
	}
	if len(got) != len(want) {
		t.Fatalf("len(k1) = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("k1[%d] = %+v, want %+v (порядок ord ASC, поля байт-в-байт)", i, got[i], w)
		}
	}

	// Повторная вставка того же (dedup_key,url) — update kind/ord, не дубль:
	// a.example получает ord=5 и переезжает в конец списка.
	if err := st.UpsertItemLinks(ctx, []ItemLink{{DedupKey: "k1", URL: "https://a.example", Kind: "unavailable", Ord: 5}}); err != nil {
		t.Fatalf("UpsertItemLinks update: %v", err)
	}
	got2, err := st.ListItemLinks(ctx, "k1")
	if err != nil {
		t.Fatalf("ListItemLinks after update: %v", err)
	}
	want2 := []ItemLink{
		{DedupKey: "k1", URL: "https://b.example", Kind: "ad", Ord: 2},
		{DedupKey: "k1", URL: "https://c.example", Kind: "other", Ord: 3},
		{DedupKey: "k1", URL: "https://a.example", Kind: "unavailable", Ord: 5},
	}
	if len(got2) != len(want2) {
		t.Fatalf("after update len = %d, want %d (upsert, not insert)", len(got2), len(want2))
	}
	for i, w := range want2 {
		if got2[i] != w {
			t.Errorf("after update k1[%d] = %+v, want %+v", i, got2[i], w)
		}
	}
	var count int
	if err := st.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM item_links").Scan(&count); err != nil {
		t.Fatalf("count item_links: %v", err)
	}
	if count != 4 {
		t.Errorf("item_links rows = %d, want 4", count)
	}

	// Байт-в-байт: кавычки/эмодзи/CRLF в dedup_key/url/kind.
	exact := ItemLink{
		DedupKey: `weird:"key'<>` + "\n",
		URL:      "https://e.example/😀\r\n",
		Kind:     "«other»",
		Ord:      7,
	}
	if err := st.UpsertItemLinks(ctx, []ItemLink{exact}); err != nil {
		t.Fatalf("UpsertItemLinks exact: %v", err)
	}
	gotExact, err := st.ListItemLinks(ctx, exact.DedupKey)
	if err != nil {
		t.Fatalf("ListItemLinks exact: %v", err)
	}
	if len(gotExact) != 1 || gotExact[0] != exact {
		t.Errorf("byte-exact roundtrip failed:\n got %+v\nwant %+v", gotExact, exact)
	}

	// Неизвестный ключ — пустой результат, не ошибка.
	missing, err := st.ListItemLinks(ctx, "no-such-key")
	if err != nil {
		t.Fatalf("ListItemLinks(missing): %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("missing key result = %+v, want empty", missing)
	}

	// Пустой батч — no-op без ошибки.
	if err := st.UpsertItemLinks(ctx, nil); err != nil {
		t.Errorf("UpsertItemLinks(nil) = %v, want nil", err)
	}
}
