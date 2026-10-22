// Дым-тест wiring'а 9D-2a (app.go): digest.NewDrillDown(store, store.ListSources)
// поверх реальной temp-SQLite — InsertDigest(sections_json) → MarkDelivered →
// S:/M: и legacy L2:/L3: data → CallbackResult; peer-Title fallback имён;
// РЕСТАРТ (закрытый и заново открытый store, новый seam) — S:/M: рендерятся из
// БД, включая саммари топиков из sections_json и блок «Ссылки:» (план 9D
// тест-кейс 7); P: → ack «Медиа недоступно»; M: на отсутствующий id → error.

package app

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/akomyagin/aiTelegaBot/internal/digest"
	"github.com/akomyagin/aiTelegaBot/internal/storage"
)

func TestDrillDownWiringSmoke9D(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "drill.db")
	st, err := storage.Open(path)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}

	topicID, err := st.CreateTopic(ctx, "AI")
	if err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	rssID, err := st.AddSource(ctx, "rss", "https://example.com/feed.xml")
	if err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	tgID, err := st.AddSource(ctx, "tg_mtproto", "-1001234567890")
	if err != nil {
		t.Fatalf("AddSource(tg): %v", err)
	}
	// Peer-кеш для fallback-имени tg-источника (план §4.6 строки 393–397).
	if err := st.UpsertPeer(ctx, 1234567890, 4242, "Приватный канал", ""); err != nil {
		t.Fatalf("UpsertPeer: %v", err)
	}
	for _, sid := range []int64{rssID, tgID} {
		if _, err := st.SetSourceTopic(ctx, sid, topicID); err != nil {
			t.Fatalf("SetSourceTopic: %v", err)
		}
	}
	sections := `[{"source_id":` + strconv.FormatInt(rssID, 10) + `,"summary":"Саммари RSS-топика"}]`
	digestID, err := st.InsertDigest(ctx, 500, topicID, "<b>body</b>", 2, sections)
	if err != nil {
		t.Fatalf("InsertDigest: %v", err)
	}
	err = st.MarkDelivered(ctx, digestID, []storage.DigestItem{{
		SourceID:  rssID,
		DedupKey:  "dk1",
		Title:     "Заголовок <1>",
		URL:       "https://x.example/a?b=1&c=2",
		Summary:   "Сводка",
		Snippet:   "Сниппет",
		FullText:  "Полный текст сообщения",
		Class:     "article",
		MediaKind: "photo",
		MediaRef:  "AgADBAAD",
	}, {
		SourceID: tgID,
		DedupKey: "dk2",
		Title:    "TG пост",
		TgPeer:   "-1001234567890",
		TgMsgID:  7,
		Summary:  "TG сводка",
		FullText: "Полный TG текст",
	}})
	if err != nil {
		t.Fatalf("MarkDelivered: %v", err)
	}
	if err := st.UpsertItemLinks(ctx, []storage.ItemLink{
		{DedupKey: "dk1", URL: "https://article.example/x", Kind: "article", Ord: 0},
	}); err != nil {
		t.Fatalf("UpsertItemLinks: %v", err)
	}
	if err := st.UpsertLinkCache(ctx, []storage.LinkCacheRow{
		{URL: "https://article.example/x", Kind: "article", Title: "Статья", Summary: "Саммари статьи"},
	}); err != nil {
		t.Fatalf("UpsertLinkCache: %v", err)
	}

	items, err := st.ListDigestItems(ctx, digestID)
	if err != nil || len(items) != 2 {
		t.Fatalf("ListDigestItems = %d items, %v", len(items), err)
	}
	itemID := items[0].ID

	// Ровно тот seam, который app.go проводит в telegram.WithDrillDown.
	dd := digest.NewDrillDown(st, st.ListSources)
	did := strconv.FormatInt(digestID, 10)
	rsid := strconv.FormatInt(rssID, 10)
	iid := strconv.FormatInt(itemID, 10)

	// Legacy «L2:<did>» → RenderTopicView: body + «Топики:» + саммари из
	// sections_json + имена (rss = Ref, tg = peer-Title) + кнопки S:/TOC:.
	l2, err := dd(ctx, "L2:"+did)
	if err != nil || l2 == nil || l2.Edit == nil {
		t.Fatalf("L2 = %+v, %v", l2, err)
	}
	for _, want := range []string{
		"<b>body</b>", "Топики:", "Саммари RSS-топика",
		"https://example.com/feed.xml", "Приватный канал",
	} {
		if !strings.Contains(l2.Edit.Text, want) {
			t.Errorf("L2 text lacks %q:\n%s", want, l2.Edit.Text)
		}
	}
	if len(l2.Edit.Buttons) != 3 ||
		l2.Edit.Buttons[0][0].Data != "S:"+did+":"+rsid ||
		l2.Edit.Buttons[1][0].Data != "S:"+did+":"+strconv.FormatInt(tgID, 10) ||
		l2.Edit.Buttons[2][0].Data != "TOC:500" {
		t.Errorf("L2 buttons = %+v", l2.Edit.Buttons)
	}

	// «S:<did>:<rssID>» → RenderSourceView: шапка + саммари топика + блоки.
	sv, err := dd(ctx, "S:"+did+":"+rsid)
	if err != nil || sv == nil || sv.Edit == nil {
		t.Fatalf("S = %+v, %v", sv, err)
	}
	for _, want := range []string{
		"📰 AI / https://example.com/feed.xml", "Саммари RSS-топика",
		`<a href="https://x.example/a?b=1&amp;c=2">Заголовок &lt;1&gt;</a>`, "Сводка",
	} {
		if !strings.Contains(sv.Edit.Text, want) {
			t.Errorf("S text lacks %q:\n%s", want, sv.Edit.Text)
		}
	}
	if len(sv.Edit.Buttons) != 2 ||
		sv.Edit.Buttons[0][0].Data != "M:"+iid ||
		sv.Edit.Buttons[1][0].Data != "T:"+did {
		t.Errorf("S buttons = %+v", sv.Edit.Buttons)
	}

	// Legacy «L3:<did>:0» → S-view sid=0 (все топики с подзаголовками).
	all, err := dd(ctx, "L3:"+did+":0")
	if err != nil || all == nil || all.Edit == nil {
		t.Fatalf("L3:0 = %+v, %v", all, err)
	}
	for _, want := range []string{"Заголовок &lt;1&gt;", "TG пост"} {
		if !strings.Contains(all.Edit.Text, want) {
			t.Errorf("L3:0 text lacks %q:\n%s", want, all.Edit.Text)
		}
	}

	// «M:<itemID>» → RenderMessageView: full_text + «Ссылки:» + кнопки.
	mv, err := dd(ctx, "M:"+iid)
	if err != nil || mv == nil || mv.Edit == nil {
		t.Fatalf("M = %+v, %v", mv, err)
	}
	for _, want := range []string{"Полный текст сообщения", "Ссылки:", "📄 Статья", "Саммари статьи"} {
		if !strings.Contains(mv.Edit.Text, want) {
			t.Errorf("M text lacks %q:\n%s", want, mv.Edit.Text)
		}
	}
	if len(mv.Edit.Buttons) != 3 {
		t.Fatalf("M buttons = %+v, want 3 rows", mv.Edit.Buttons)
	}
	if mv.Edit.Buttons[0][0].Data != "P:"+iid {
		t.Errorf("M row0 = %+v, want 🖼 Фото P:%s", mv.Edit.Buttons[0], iid)
	}
	if u := mv.Edit.Buttons[1][0]; u.URL != "https://x.example/a?b=1&c=2" || u.Data != "" {
		t.Errorf("M row1 = %+v, want URL-кнопку", u)
	}
	if mv.Edit.Buttons[2][0].Data != "S:"+did+":"+rsid {
		t.Errorf("M row2 = %+v, want ◀ Назад S:%s:%s", mv.Edit.Buttons[2], did, rsid)
	}

	// «M:» на отсутствующий id → error (ack «Не получилось открыть»), не паника.
	if res, err := dd(ctx, "M:99999999"); err == nil || res != nil {
		t.Errorf("M:missing = %+v, %v, want error/nil", res, err)
	}
	// «P:» → ack-заглушка «Медиа недоступно» (фото в 9E), без Edit.
	pv, err := dd(ctx, "P:"+iid)
	if err != nil || pv == nil || pv.AckText != "Медиа недоступно" || pv.Edit != nil {
		t.Errorf("P = %+v, %v, want ack «Медиа недоступно»", pv, err)
	}

	// РЕСТАРТ (тест-кейс 7 плана): store закрыт и открыт заново, новый seam —
	// S:/M:/L2: рендерятся из БД, включая sections_json-саммари и «Ссылки:».
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	st2, err := storage.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	dd2 := digest.NewDrillDown(st2, st2.ListSources)

	s2, err := dd2(ctx, "S:"+did+":"+rsid)
	if err != nil || s2 == nil || s2.Edit == nil {
		t.Fatalf("restart S = %+v, %v", s2, err)
	}
	if !strings.Contains(s2.Edit.Text, "Саммари RSS-топика") {
		t.Errorf("restart S text lacks sections_json summary:\n%s", s2.Edit.Text)
	}
	m2, err := dd2(ctx, "M:"+iid)
	if err != nil || m2 == nil || m2.Edit == nil {
		t.Fatalf("restart M = %+v, %v", m2, err)
	}
	for _, want := range []string{"Полный текст сообщения", "Ссылки:", "📄 Статья"} {
		if !strings.Contains(m2.Edit.Text, want) {
			t.Errorf("restart M text lacks %q:\n%s", want, m2.Edit.Text)
		}
	}
	l22, err := dd2(ctx, "L2:"+did)
	if err != nil || l22 == nil || l22.Edit == nil || !strings.Contains(l22.Edit.Text, "Топики:") {
		t.Fatalf("restart L2 = %+v, %v", l22, err)
	}
}
