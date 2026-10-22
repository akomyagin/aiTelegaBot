// Интеграционные тесты шага медиа (подэтап 9E, план тест-кейс №5): реальный
// Pipeline.Run поверх фейков и временной SQLite (SKILL §7) — tg_mtproto
// photo-элемент → digest_items.media_ref + кнопка «🖼 Фото» в L3; botapi
// file_id — без resolver'а; кеш message_state → Resolve не вызван;
// Media == nil (offline/no-mtClient) → Run без изменений; бюджет re-upload'ов
// ОБЩИЙ на прогон (план §4.8.1: счётчик в pipeline).

package digest

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
	"github.com/akomyagin/aiTelegaBot/internal/llm"
	"github.com/akomyagin/aiTelegaBot/internal/storage"
)

// fakeMediaResolver — фейк MediaResolver: заполняет MediaFileID у tg_mtproto
// photo-элементов без ref в пределах budget, записывает вызовы и полученные
// бюджеты (проверка общего счётчика на прогон).
type fakeMediaResolver struct {
	calls        int
	budgets      []int
	resolvedKeys []string
	fileID       string // file_id = <fileID>-<item.ID>
	err          error
}

func (f *fakeMediaResolver) Resolve(_ context.Context, items []feed.Item, budget int) (int, error) {
	f.calls++
	f.budgets = append(f.budgets, budget)
	used := 0
	for i := range items {
		it := &items[i]
		if it.Kind != "tg_mtproto" || it.MediaKind != "photo" ||
			it.MediaFileID != "" || it.MediaURL != "" {
			continue
		}
		if used >= budget {
			break
		}
		it.MediaFileID = f.fileID + "-" + it.ID
		f.resolvedKeys = append(f.resolvedKeys, it.DedupKey())
		used++
	}
	return used, f.err
}

func mtPhotoItem9E(msgID int) feed.Item {
	return feed.Item{
		Kind: "tg_mtproto", Source: "@privchan", ID: strconv.Itoa(msgID),
		Title: fmt.Sprintf("m-T%d", msgID), URL: fmt.Sprintf("https://t.me/c/100/%d", msgID),
		Text: fmt.Sprintf("private text %d", msgID), MediaKind: "photo",
	}
}

func botapiPhotoItem9E(msgID int) feed.Item {
	return feed.Item{
		Kind: "tg_botapi", Source: "@pubchan", ID: strconv.Itoa(msgID),
		Title: fmt.Sprintf("b-T%d", msgID), URL: fmt.Sprintf("https://t.me/pubchan/%d", msgID),
		Text: "managed text", MediaKind: "photo", MediaFileID: "fid-botapi",
	}
}

// openTestStoreAt — openTestStore, но дополнительно возвращает путь БД:
// снапшот digest_items проверяется прямым SQL-чтением.
func openTestStoreAt(t *testing.T) (*storage.SQLiteStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "media.db")
	st, err := storage.Open(path)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, path
}

type mediaRow struct {
	id        int64
	kind, ref string
}

// digestItemMedia — снапшот digest_items (per-run, навигация P: читает его):
// dedup_key → id + media_kind/media_ref.
func digestItemMedia(t *testing.T, dbPath string) map[string]mediaRow {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT id, dedup_key, media_kind, media_ref FROM digest_items`)
	if err != nil {
		t.Fatalf("query digest_items: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]mediaRow)
	for rows.Next() {
		var key string
		var r mediaRow
		if err := rows.Scan(&r.id, &key, &r.kind, &r.ref); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[key] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// hasPhotoButton — в L3-вью есть кнопка «🖼 Фото» (P:<id>) / её нет.
func hasPhotoButton(t *testing.T, st *storage.SQLiteStore, itemID int64) bool {
	t.Helper()
	it, ok, err := st.GetDigestItem(context.Background(), itemID)
	if err != nil || !ok {
		t.Fatalf("GetDigestItem(%d): ok=%v err=%v", itemID, ok, err)
	}
	view := RenderMessageView(it, nil)
	want := "P:" + strconv.FormatInt(itemID, 10)
	for _, row := range view.Buttons {
		for _, b := range row {
			if b.Data == want {
				return b.Text == messagePhotoLabel
			}
		}
	}
	return false
}

func TestPipelineRun_MediaResolveAndSnapshot(t *testing.T) {
	st, dbPath := openTestStoreAt(t)
	ctx := context.Background()
	items := []feed.Item{mtPhotoItem9E(42), botapiPhotoItem9E(7)}
	p := newTestPipeline(st, &fakeClassifier{}, &fakePoster{},
		[]TrackedSource{{Src: fakeSource{name: "src", items: items}, SourceID: 1, TopicID: 1}}, testNow)
	res := &fakeMediaResolver{fileID: "fid"}
	p.Media = res
	p.MediaReuploadMax = 3

	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Resolver вызван один раз (тема одна) и только по mtproto-элементу;
	// botapi-элемент с file_id от источника в Resolve НЕ participates.
	if res.calls != 1 {
		t.Errorf("Resolve calls = %d, want 1", res.calls)
	}
	if len(res.resolvedKeys) != 1 || res.resolvedKeys[0] != "tg:@privchan:42" {
		t.Errorf("resolvedKeys = %v, want [tg:@privchan:42]", res.resolvedKeys)
	}

	// digest_items: снапшот media_kind/media_ref (file_id от resolver'а и
	// file_id источника).
	media := digestItemMedia(t, dbPath)
	mt := media["tg:@privchan:42"]
	if mt.kind != "photo" || mt.ref != "fid-42" {
		t.Errorf("mtproto digest_items media = %q/%q, want photo/fid-42", mt.kind, mt.ref)
	}
	if bo := media["tg:@pubchan:7"]; bo.kind != "photo" || bo.ref != "fid-botapi" {
		t.Errorf("botapi digest_items media = %q/%q, want photo/fid-botapi", bo.kind, bo.ref)
	}

	// message_state: кеш media_ref записан — следующий прогон не скачивает.
	states, err := st.GetMessageStates(ctx, []string{"tg:@privchan:42", "tg:@pubchan:7"})
	if err != nil {
		t.Fatalf("GetMessageStates: %v", err)
	}
	if s := states["tg:@privchan:42"]; s.MediaKind != "photo" || s.MediaRef != "fid-42" {
		t.Errorf("message_state mtproto media = %q/%q, want photo/fid-42", s.MediaKind, s.MediaRef)
	}
	if s := states["tg:@pubchan:7"]; s.MediaKind != "photo" || s.MediaRef != "fid-botapi" {
		t.Errorf("message_state botapi media = %q/%q, want photo/fid-botapi", s.MediaKind, s.MediaRef)
	}

	// L3-рендер: у элемента с media_ref кнопка «🖼 Фото» (P:), у элемента
	// без — её нет (в этом тесте оба с медиа).
	if !hasPhotoButton(t, st, mt.id) {
		t.Error("L3 mtproto-элемента: нет кнопки «🖼 Фото» (P:)")
	}
}

func TestPipelineRun_MediaCacheHit(t *testing.T) {
	st, dbPath := openTestStoreAt(t)
	ctx := context.Background()
	// «Прошлый прогон» уже резолвил фото: done-строка с media_ref.
	if err := st.UpsertMessageState(ctx, []storage.MessageState{{
		DedupKey: "tg:@privchan:42", SourceID: 1, TopicID: 1,
		Class: llm.ClassOther, Summary: "cached summary", IsPrimary: true,
		Status: "done", MediaKind: "photo", MediaRef: "fid-cached",
	}}); err != nil {
		t.Fatalf("UpsertMessageState: %v", err)
	}
	p := newTestPipeline(st, &fakeClassifier{}, &fakePoster{},
		[]TrackedSource{{Src: fakeSource{name: "src", items: []feed.Item{mtPhotoItem9E(42)}}, SourceID: 1, TopicID: 1}}, testNow)
	res := &fakeMediaResolver{fileID: "fid"}
	p.Media = res
	p.MediaReuploadMax = 3

	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Кеш-попадание: Resolve НЕ вызван, download не выполняется.
	if res.calls != 0 {
		t.Errorf("Resolve calls = %d, want 0 (кеш message_state)", res.calls)
	}
	// Снапшот digest_items несёт ЗАКЕШИРОВАННЫЙ file_id.
	if m := digestItemMedia(t, dbPath)["tg:@privchan:42"]; m.ref != "fid-cached" {
		t.Errorf("digest_items media_ref = %q, want fid-cached", m.ref)
	}
	// Повторная запись message_state не нужна: строка не изменилась —
	// проверяем, что media_ref остался прежним.
	states, err := st.GetMessageStates(ctx, []string{"tg:@privchan:42"})
	if err != nil {
		t.Fatalf("GetMessageStates: %v", err)
	}
	if s := states["tg:@privchan:42"]; s.MediaRef != "fid-cached" || s.Summary != "cached summary" {
		t.Errorf("message_state = ref %q summary %q, want fid-cached/cached summary", s.MediaRef, s.Summary)
	}
}

func TestPipelineRun_MediaNoResolver(t *testing.T) {
	st, dbPath := openTestStoreAt(t)
	items := []feed.Item{mtPhotoItem9E(42), botapiPhotoItem9E(7)}
	p := newTestPipeline(st, &fakeClassifier{}, &fakePoster{},
		[]TrackedSource{{Src: fakeSource{name: "src", items: items}, SourceID: 1, TopicID: 1}}, testNow)
	// Media == nil (offline / нет mtClient) + MediaReuploadMax>0 — Run не
	// меняется: медиа только от источников.
	p.MediaReuploadMax = 3

	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	media := digestItemMedia(t, dbPath)
	if m := media["tg:@privchan:42"]; m.ref != "" {
		t.Errorf("mtproto media_ref = %q, want '' (resolver no-op)", m.ref)
	}
	if m := media["tg:@pubchan:7"]; m.ref != "fid-botapi" {
		t.Errorf("botapi media_ref = %q, want fid-botapi", m.ref)
	}
	// L3 mtproto-элемента БЕЗ media_ref: кнопки «🖼 Фото» нет (деградация).
	if hasPhotoButton(t, st, media["tg:@privchan:42"].id) {
		t.Error("L3 без media_ref: появилась кнопка «🖼 Фото»")
	}
	// L3 botapi-элемента: кнопка есть.
	if !hasPhotoButton(t, st, media["tg:@pubchan:7"].id) {
		t.Error("L3 botapi-элемента: нет кнопки «🖼 Фото»")
	}
}

func TestPipelineRun_MediaBudgetSharedAcrossTopics(t *testing.T) {
	st, dbPath := openTestStoreAt(t)
	srcA := fakeSource{name: "A", items: []feed.Item{mtPhotoItem9E(1)}}
	srcB := fakeSource{name: "B", items: []feed.Item{mtPhotoItem9E(2)}}
	p := newTestPipeline(st, &fakeClassifier{}, &fakePoster{}, []TrackedSource{
		{Src: srcA, SourceID: 1, TopicID: 2},
		{Src: srcB, SourceID: 2, TopicID: 3},
	}, testNow)
	res := &fakeMediaResolver{fileID: "fid"}
	p.Media = res
	p.MediaReuploadMax = 1 // ОБЩИЙ бюджет на прогон: вторая тема получит 0

	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Первая тема израсходовала бюджет (1); для второй Resolve НЕ вызывается
	// вовсе (pipeline-гейт *budget>0 — тратить нечего), счётчик общий на
	// прогон (план §4.8.1).
	if res.calls != 1 {
		t.Errorf("Resolve calls = %d, want 1", res.calls)
	}
	if len(res.budgets) != 1 || res.budgets[0] != 1 {
		t.Errorf("budgets = %v, want [1] — счётчик общий на прогон", res.budgets)
	}
	media := digestItemMedia(t, dbPath)
	if m := media["tg:@privchan:1"]; m.ref != "fid-1" {
		t.Errorf("тема 2: media_ref = %q, want fid-1", m.ref)
	}
	if m := media["tg:@privchan:2"]; m.ref != "" {
		t.Errorf("тема 3: media_ref = %q, want '' (бюджет исчерпан)", m.ref)
	}
}
