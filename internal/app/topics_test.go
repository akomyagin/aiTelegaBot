package app

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
	"github.com/akomyagin/aiTelegaBot/internal/llm"
	"github.com/akomyagin/aiTelegaBot/internal/storage"
	"github.com/akomyagin/aiTelegaBot/internal/telegram"
)

// fakeTopicSuggester — фейк llm.TopicSuggester без сети: возвращает
// запрограммированные имя/ошибку и записывает вход для проверок.
type fakeTopicSuggester struct {
	name         string
	err          error
	gotExisting  []string
	gotSampleLen int
	calls        int
}

func (f *fakeTopicSuggester) SuggestTopic(_ context.Context, existing []string, sample []feed.Item) (string, error) {
	f.calls++
	f.gotExisting = existing
	f.gotSampleLen = len(sample)
	return f.name, f.err
}

var _ llm.TopicSuggester = (*fakeTopicSuggester)(nil)

// TestRegisterStaticSources: строки origin='static' появляются после
// регистрации; повторный старт (та же функция) НЕ затирает topic_id и не
// плодит дубли (идемпотентность UpsertStaticSource, план: «static upsert не
// затирает topic_id при повторном старте»).
func TestRegisterStaticSources(t *testing.T) {
	st := newForwardTestStore(t)
	ctx := context.Background()

	refs := []staticSourceRef{
		{Kind: "rss", Ref: "https://example.com/feed.xml"},
		{Kind: "arxiv", Ref: "http://export.arxiv.org/api/query?search_query=cat:cs.CL"},
		{Kind: "hn", Ref: "frontpage"},
		{Kind: "tg_botapi", Ref: "managedchan"},
		{Kind: "tg_public", Ref: "publicchan"},
		{Kind: "tg_mtproto", Ref: "privchan"},
	}
	registerStaticSources(ctx, st, refs)

	rows, err := st.ListSources(ctx)
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	if len(rows) != len(refs) {
		t.Fatalf("got %d rows, want %d: %+v", len(rows), len(refs), rows)
	}
	byRef := map[string]storage.Source{}
	for _, r := range rows {
		if r.Origin != "static" || !r.Enabled {
			t.Errorf("row %q: origin=%q enabled=%v, want static/enabled", r.Ref, r.Origin, r.Enabled)
		}
		byRef[r.Ref] = r
	}
	for _, ref := range refs {
		if _, ok := byRef[ref.Ref]; !ok {
			t.Errorf("ref %q not registered", ref.Ref)
		}
	}

	// Привязываем тему к одному статическому источнику и «перезапускаем»
	// регистрацию: topic_id должен пережить повторный upsert.
	topicID, err := st.CreateTopic(ctx, "AI")
	if err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	arxiv := byRef["http://export.arxiv.org/api/query?search_query=cat:cs.CL"]
	if ok, err := st.SetSourceTopic(ctx, arxiv.ID, topicID); err != nil || !ok {
		t.Fatalf("SetSourceTopic: ok=%v err=%v", ok, err)
	}

	registerStaticSources(ctx, st, refs) // повторный старт

	rows2, err := st.ListSources(ctx)
	if err != nil {
		t.Fatalf("ListSources(2): %v", err)
	}
	if len(rows2) != len(refs) {
		t.Fatalf("second registration created duplicates: %d rows, want %d", len(rows2), len(refs))
	}
	for _, r := range rows2 {
		if r.Ref == arxiv.Ref {
			if r.TopicID != topicID {
				t.Errorf("topic_id after re-register = %d, want %d", r.TopicID, topicID)
			}
			if r.ID != arxiv.ID {
				t.Errorf("row id changed after re-register: %d -> %d", arxiv.ID, r.ID)
			}
		}
	}
}

// TestBuildDynamicSources_SkipsStatic: строки origin='static' не строятся в
// динамическом слое (иначе дубли со статическим слоем из config, план 614–615).
func TestBuildDynamicSources_SkipsStatic(t *testing.T) {
	rows := []storage.Source{
		{ID: 1, Kind: "rss", Ref: "https://example.com/dyn.xml", Enabled: true, Origin: "dynamic"},
		{ID: 2, Kind: "rss", Ref: "https://example.com/static.xml", Enabled: true, Origin: "static"},
		{ID: 3, Kind: "tg_botapi", Ref: "managed", Enabled: true, Origin: "static"},
		{ID: 4, Kind: "tg_botapi", Ref: "dynmanaged", Enabled: true, Origin: ""}, // legacy-строка без origin
	}
	out := buildDynamicSources(rows, http.DefaultClient, 20, &telegram.ChannelBuffer{}, nil, 20)
	if len(out) != 2 {
		t.Fatalf("got %d sources, want 2 (static skipped): %v", len(out), names(out))
	}
	if out[0].Src.Name() != "https://example.com/dyn.xml" {
		t.Errorf("source[0] = %q, want the dynamic rss ref", out[0].Src.Name())
	}
	if out[1].Src.Name() != "@dynmanaged" {
		t.Errorf("source[1] = %q, want @dynmanaged", out[1].Src.Name())
	}
}

// TestNewTopicBinder: числовой id / точное имя / source not found / topic not
// found; успешная привязка видна в ListSources.
func TestNewTopicBinder(t *testing.T) {
	st := newForwardTestStore(t)
	ctx := context.Background()
	binder := newTopicBinder(st)

	srcID, _, _, err := st.AddSourceIfNotExists(ctx, "rss", "https://example.com/f.xml")
	if err != nil {
		t.Fatalf("AddSourceIfNotExists: %v", err)
	}
	topicID, err := st.CreateTopic(ctx, "AI")
	if err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}

	tests := []struct {
		name        string
		sourceID    int64
		topicArg    string
		wantSrc     bool
		wantTopic   bool
		wantTopicID int64
		wantErr     bool
	}{
		{name: "числовой id темы", sourceID: srcID, topicArg: "2", wantSrc: true, wantTopic: true, wantTopicID: topicID},
		{name: "точное имя темы", sourceID: srcID, topicArg: "AI", wantSrc: true, wantTopic: true, wantTopicID: topicID},
		{name: "источник не найден", sourceID: 999, topicArg: "2", wantSrc: false, wantTopic: false},
		{name: "тема-id не найдена", sourceID: srcID, topicArg: "42", wantSrc: true, wantTopic: false},
		{name: "тема-имя не найдена", sourceID: srcID, topicArg: "НетТакой", wantSrc: true, wantTopic: false},
		{name: "имя с ведущим пробелом не матчится", sourceID: srcID, topicArg: " AI", wantSrc: true, wantTopic: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := binder(ctx, tt.sourceID, tt.topicArg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr=%v", err, tt.wantErr)
			}
			if res.SourceFound != tt.wantSrc || res.TopicFound != tt.wantTopic {
				t.Fatalf("res = %+v, want SourceFound=%v TopicFound=%v", res, tt.wantSrc, tt.wantTopic)
			}
			if tt.wantTopic && res.TopicID != tt.wantTopicID {
				t.Errorf("TopicID = %d, want %d", res.TopicID, tt.wantTopicID)
			}
			if tt.wantTopic && res.TopicName != "AI" {
				t.Errorf("TopicName = %q, want %q", res.TopicName, "AI")
			}
		})
	}

	// Успешная привязка действительно записана (последний успешный кейс —
	// «имя с пробелом» не связывал; свяжем явно и проверим).
	if _, err := binder(ctx, srcID, "AI"); err != nil {
		t.Fatalf("binder: %v", err)
	}
	srcs, err := st.ListSources(ctx)
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	if len(srcs) != 1 || srcs[0].TopicID != topicID {
		t.Fatalf("binding not persisted: %+v", srcs)
	}
}

// TestNewTopicCreator: Created различает «создана»/«уже была» (контракт b-tg
// §2), id идемпотентен.
func TestNewTopicCreator(t *testing.T) {
	st := newForwardTestStore(t)
	ctx := context.Background()
	creator := newTopicCreator(st)

	first, err := creator(ctx, "Новости")
	if err != nil {
		t.Fatalf("creator: %v", err)
	}
	if !first.Created || first.ID == 0 {
		t.Fatalf("first = %+v, want Created=true, non-zero ID", first)
	}
	second, err := creator(ctx, "Новости")
	if err != nil {
		t.Fatalf("creator(2): %v", err)
	}
	if second.Created {
		t.Errorf("second.Created = true, want false (уже была)")
	}
	if second.ID != first.ID {
		t.Errorf("second.ID = %d, want %d", second.ID, first.ID)
	}
}

// TestNewTopicLister: группировка источников по COALESCE(topic_id,1),
// порядок — как из ListTopics, disabled-источники видны.
func TestNewTopicLister(t *testing.T) {
	st := newForwardTestStore(t)
	ctx := context.Background()
	lister := newTopicLister(st)

	aiID, err := st.CreateTopic(ctx, "AI")
	if err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	dynID, _, _, err := st.AddSourceIfNotExists(ctx, "rss", "https://example.com/a.xml")
	if err != nil {
		t.Fatalf("AddSourceIfNotExists: %v", err)
	}
	boundID, _, _, err := st.AddSourceIfNotExists(ctx, "tg_public", "chan")
	if err != nil {
		t.Fatalf("AddSourceIfNotExists: %v", err)
	}
	if ok, err := st.SetSourceTopic(ctx, boundID, aiID); err != nil || !ok {
		t.Fatalf("SetSourceTopic: ok=%v err=%v", ok, err)
	}
	if err := st.UpsertStaticSource(ctx, "hn", "frontpage"); err != nil {
		t.Fatalf("UpsertStaticSource: %v", err)
	}
	offID, _, _, err := st.AddSourceIfNotExists(ctx, "rss", "https://example.com/off.xml")
	if err != nil {
		t.Fatalf("AddSourceIfNotExists: %v", err)
	}
	if _, err := st.DisableSource(ctx, offID); err != nil {
		t.Fatalf("DisableSource: %v", err)
	}

	views, err := lister(ctx)
	if err != nil {
		t.Fatalf("lister: %v", err)
	}
	if len(views) != 2 {
		t.Fatalf("got %d topics, want 2 (seed + AI): %+v", len(views), views)
	}
	if views[0].ID != 1 || views[0].Name != "Без темы" {
		t.Errorf("views[0] = {%d,%q}, want seed {1,Без темы}", views[0].ID, views[0].Name)
	}
	// «Без темы»: dyn (NULL→1), static hn, disabled off — все трое.
	if len(views[0].Sources) != 3 {
		t.Errorf("seed sources = %d, want 3: %+v", len(views[0].Sources), views[0].Sources)
	}
	hasDisabled := false
	for _, s := range views[0].Sources {
		if s.ID == dynID && (s.Kind != "rss" || !s.Enabled) {
			t.Errorf("dyn source view = %+v", s)
		}
		if s.ID == offID && !s.Enabled {
			hasDisabled = true
		}
	}
	if !hasDisabled {
		t.Errorf("disabled source missing from the list: %+v", views[0].Sources)
	}
	if views[1].ID != aiID || len(views[1].Sources) != 1 || views[1].Sources[0].ID != boundID {
		t.Errorf("views[1] = %+v, want AI with the bound tg_public source", views[1])
	}
}

// TestNewTopicSuggester: фейк → CreateTopic вызван, suggestion возвращён;
// пустое имя / ошибка / sourceID=0 / offline → nil (деградация без падения
// добавления). Sample для tg_botapi — всегда пусто (дренаж channelBuf
// запрещён), существующие темы передаются без seed «Без темы».
func TestNewTopicSuggester(t *testing.T) {
	tests := []struct {
		name       string
		sourceID   int64
		suggester  llm.TopicSuggester
		wantNil    bool
		wantName   string
		wantCalls  int
		wantExist  []string // nil => не проверяем
		wantSample int
	}{
		{
			name: "фейк предложил имя — тема создана сразу", sourceID: 5,
			suggester: &fakeTopicSuggester{name: "ML"},
			wantName:  "ML", wantCalls: 1, wantExist: []string{"AI"}, wantSample: 0,
		},
		{
			name: "пустое имя — nil", sourceID: 5,
			suggester: &fakeTopicSuggester{name: ""}, wantNil: true, wantCalls: 1,
		},
		{
			name: "ошибка suggester — nil, не фатально", sourceID: 5,
			suggester: &fakeTopicSuggester{err: errors.New("boom")}, wantNil: true, wantCalls: 1,
		},
		{
			name: "sourceID=0 — без вызова", sourceID: 0,
			suggester: &fakeTopicSuggester{name: "ML"}, wantNil: true, wantCalls: 0,
		},
		{
			name: "offline-суммаризатор — nil (без LLM-ключа добавление работает)", sourceID: 5,
			suggester: llm.NewOffline(), wantNil: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newForwardTestStore(t)
			ctx := context.Background()
			if _, err := st.CreateTopic(ctx, "AI"); err != nil {
				t.Fatalf("CreateTopic: %v", err)
			}
			// kind=tg_botapi: sample заведомо пустой, без сети и без дренажа
			// channelBuf — тест проверяет seam, а не коллекторы.
			sug := newTopicSuggester(st, tt.suggester)
			got := sug(ctx, tt.sourceID, "tg_botapi", "chan", nil)

			if tt.wantNil {
				if got != nil {
					t.Fatalf("got %+v, want nil", got)
				}
			} else {
				if got == nil {
					t.Fatalf("got nil, want suggestion %q", tt.wantName)
				}
				if got.TopicName != tt.wantName || got.TopicID == 0 {
					t.Fatalf("got %+v, want {%d,%q}", got, got.TopicID, tt.wantName)
				}
				// Тема создана СРАЗУ (§4.11 шаг 3).
				topics, err := st.ListTopics(ctx)
				if err != nil {
					t.Fatalf("ListTopics: %v", err)
				}
				found := false
				for _, tp := range topics {
					if tp.Name == tt.wantName && tp.ID == got.TopicID {
						found = true
					}
				}
				if !found {
					t.Errorf("suggested topic not persisted: %+v", topics)
				}
			}

			if f, ok := tt.suggester.(*fakeTopicSuggester); ok {
				if f.calls != tt.wantCalls {
					t.Errorf("SuggestTopic calls = %d, want %d", f.calls, tt.wantCalls)
				}
				if tt.wantExist != nil && !equalStrings(f.gotExisting, tt.wantExist) {
					t.Errorf("existing = %v, want %v (seed «Без темы» исключён)", f.gotExisting, tt.wantExist)
				}
				if f.calls > 0 && f.gotSampleLen != tt.wantSample {
					t.Errorf("sample len = %d, want %d (tg_botapi — всегда пусто)", f.gotSampleLen, tt.wantSample)
				}
			}
		})
	}
}

// TestNewTopicSuggester_CaseInsensitiveExisting (ревью L1): модель вернула
// имя, отличающееся от существующего регистром/пробелами → возвращается
// существующая тема, near-duplicate НЕ создаётся.
func TestNewTopicSuggester_CaseInsensitiveExisting(t *testing.T) {
	st := newForwardTestStore(t)
	ctx := context.Background()
	aiID, err := st.CreateTopic(ctx, "AI News")
	if err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	sug := newTopicSuggester(st, &fakeTopicSuggester{name: "  ai news "})
	got := sug(ctx, 5, "rss", "https://x", nil)
	if got == nil {
		t.Fatalf("got nil, want the existing topic")
	}
	if got.TopicID != aiID || got.TopicName != "AI News" {
		t.Errorf("got %+v, want {%d, «AI News»} — существующая тема", got, aiID)
	}
	topics, err := st.ListTopics(ctx)
	if err != nil {
		t.Fatalf("ListTopics: %v", err)
	}
	// seed «Без темы» + «AI News»: новая near-duplicate тема не появилась.
	if len(topics) != 2 {
		t.Errorf("topics = %+v, want 2 (near-duplicate не создана)", topics)
	}
}

// TestNewTopicSuggester_SamplePassThrough (ревью M3): sample из пути
// добавления передаётся в SuggestTopic как есть — seam ничего не собирает.
func TestNewTopicSuggester_SamplePassThrough(t *testing.T) {
	st := newForwardTestStore(t)
	ctx := context.Background()
	fake := &fakeTopicSuggester{name: ""}
	sug := newTopicSuggester(st, fake)
	sample := []feed.Item{{Title: "a"}, {Title: "b"}}
	if got := sug(ctx, 5, "tg_public", "chan", sample); got != nil {
		t.Fatalf("got %+v, want nil (пустое имя)", got)
	}
	if fake.gotSampleLen != 2 {
		t.Errorf("sample len = %d, want 2 (проброс из пути добавления)", fake.gotSampleLen)
	}
}

// TestNewTopicBinder_NumericNameFallback (ревью L2): числовой topicArg
// трактуется как id, но при отсутствии темы с таким id — fallback на ТОЧНОЕ
// имя: тема «2024» достижима. Приоритет id сохраняется.
func TestNewTopicBinder_NumericNameFallback(t *testing.T) {
	st := newForwardTestStore(t)
	ctx := context.Background()
	binder := newTopicBinder(st)

	srcID, _, _, err := st.AddSourceIfNotExists(ctx, "rss", "https://example.com/f.xml")
	if err != nil {
		t.Fatalf("AddSourceIfNotExists: %v", err)
	}
	yearID, err := st.CreateTopic(ctx, "2024")
	if err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}

	// Темы с id=2024 нет → fallback на точное имя «2024».
	res, err := binder(ctx, srcID, "2024")
	if err != nil {
		t.Fatalf("binder: %v", err)
	}
	if !res.SourceFound || !res.TopicFound || res.TopicID != yearID || res.TopicName != "2024" {
		t.Fatalf("res = %+v, want привязку к теме «2024» (#%d)", res, yearID)
	}
	// id имеет приоритет: «1» → seed-тема «Без темы», а не поиск по имени.
	res2, err := binder(ctx, srcID, "1")
	if err != nil {
		t.Fatalf("binder(1): %v", err)
	}
	if !res2.TopicFound || res2.TopicID != 1 {
		t.Fatalf("id должен иметь приоритет над именем: %+v", res2)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestNewSourceRemoverV2: static → {Found:true, Static:true} БЕЗ отключения;
// dynamic → DisableSource; неизвестный id → {false,false}.
func TestNewSourceRemoverV2(t *testing.T) {
	st := newForwardTestStore(t)
	ctx := context.Background()
	remover := newSourceRemoverV2(st)

	if err := st.UpsertStaticSource(ctx, "rss", "https://example.com/static.xml"); err != nil {
		t.Fatalf("UpsertStaticSource: %v", err)
	}
	dynID, _, _, err := st.AddSourceIfNotExists(ctx, "rss", "https://example.com/dyn.xml")
	if err != nil {
		t.Fatalf("AddSourceIfNotExists: %v", err)
	}
	rows, err := st.ListSources(ctx)
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	var staticID int64
	for _, r := range rows {
		if r.Origin == "static" {
			staticID = r.ID
		}
	}

	t.Run("static не отключается", func(t *testing.T) {
		res, err := remover(ctx, staticID)
		if err != nil {
			t.Fatalf("remover: %v", err)
		}
		if !res.Found || !res.Static {
			t.Fatalf("res = %+v, want {Found:true,Static:true}", res)
		}
		srcs, _ := st.ListSources(ctx)
		for _, s := range srcs {
			if s.ID == staticID && !s.Enabled {
				t.Errorf("static source was disabled: %+v", s)
			}
		}
	})
	t.Run("dynamic отключается", func(t *testing.T) {
		res, err := remover(ctx, dynID)
		if err != nil {
			t.Fatalf("remover: %v", err)
		}
		if !res.Found || res.Static {
			t.Fatalf("res = %+v, want {Found:true,Static:false}", res)
		}
		srcs, _ := st.ListSources(ctx)
		for _, s := range srcs {
			if s.ID == dynID && s.Enabled {
				t.Errorf("dynamic source still enabled: %+v", s)
			}
		}
	})
	t.Run("не найден", func(t *testing.T) {
		res, err := remover(ctx, 12345)
		if err != nil {
			t.Fatalf("remover: %v", err)
		}
		if res.Found || res.Static {
			t.Fatalf("res = %+v, want {Found:false,Static:false}", res)
		}
	})
}
