// Тесты D-2 (план §D, тест-кейсы D): парсер callback_data (мусор/overflow →
// error, не паника), golden-рендеры L2/L3, guard 4096 («…и ещё K», полоса
// M1), несуществующий digestID → error. Golden — compareGolden-паттерн
// render_topic_test.go (`go test ./internal/digest -update`).

package digest

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/akomyagin/aiTelegaBot/internal/storage"
	"github.com/akomyagin/aiTelegaBot/internal/telegram"
)

// drillStore — фейк storage.Store для drill-down: используются только
// GetDigest/ListDigestItems/ListDigestItemsBySource/ListTopics, остальное
// паникует через nil-embedding (осознанно: seam их вызывать не должен).
type drillStore struct {
	storage.Store
	digestRow storage.Digest
	hasDigest bool
	items     []storage.DigestItem
	topics    []storage.Topic
}

func (s *drillStore) GetDigest(ctx context.Context, digestID int64) (storage.Digest, bool, error) {
	return s.digestRow, s.hasDigest, nil
}

func (s *drillStore) ListDigestItems(ctx context.Context, digestID int64) ([]storage.DigestItem, error) {
	return s.items, nil
}

func (s *drillStore) ListDigestItemsBySource(ctx context.Context, digestID, sourceID int64) ([]storage.DigestItem, error) {
	var out []storage.DigestItem
	for _, it := range s.items {
		if it.SourceID == sourceID {
			out = append(out, it)
		}
	}
	return out, nil
}

func (s *drillStore) ListTopics(ctx context.Context) ([]storage.Topic, error) {
	return s.topics, nil
}

func staticSources(rows ...storage.Source) func(context.Context) ([]storage.Source, error) {
	return func(context.Context) ([]storage.Source, error) { return rows, nil }
}

func TestParseDrillDownData(t *testing.T) {
	valid := []struct {
		data     string
		level    int
		digestID int64
		sourceID int64
	}{
		{"L2:42", 2, 42, 0},
		{"L2:1", 2, 1, 0},
		{"L3:42:7", 3, 42, 7},
		{"L3:42:0", 3, 42, 0},
		{"L3:9007199254740991:9007199254740991", 3, 9007199254740991, 9007199254740991},
	}
	for _, tt := range valid {
		t.Run("ok/"+tt.data, func(t *testing.T) {
			level, did, sid, err := parseDrillDownData(tt.data)
			if err != nil {
				t.Fatalf("parseDrillDownData(%q): %v", tt.data, err)
			}
			if level != tt.level || did != tt.digestID || sid != tt.sourceID {
				t.Errorf("parseDrillDownData(%q) = (%d,%d,%d), want (%d,%d,%d)",
					tt.data, level, did, sid, tt.level, tt.digestID, tt.sourceID)
			}
		})
	}

	bad := []string{
		"",                          // пустая строка
		"L2:",                       // пустой id
		"L2:abc",                    // не число
		"L2:42:7",                   // лишний сегмент
		"L2:-1",                     // отрицательный digestID
		"L2:0",                      // нулевой digestID
		"L3:1",                      // нет sourceID
		"L3:1:x",                    // нечисловой sourceID
		"L3:1:-2",                   // отрицательный sourceID
		"L3::1",                     // пустой digestID
		"L4:1",                      // неизвестный префикс
		"l2:42",                     // регистрозависимый CutPrefix
		"T:42",                      // чужой префикс (диспетчер C)
		"L2:99999999999999999999",   // overflow int64
		"L3:99999999999999999999:1", // overflow digestID
		"L3:1:99999999999999999999", // overflow sourceID
		"L2: 42",                    // пробел
	}
	for _, data := range bad {
		t.Run("err/"+data, func(t *testing.T) {
			if _, _, _, err := parseDrillDownData(data); err == nil {
				t.Fatalf("parseDrillDownData(%q): want error, got nil", data)
			}
		})
	}
}

func TestNewDrillDown_ParseAndNotFoundErrors(t *testing.T) {
	ctx := context.Background()
	// Мусорные data → error даже без обращения к store (nil-store не паникует:
	// парсер отказывает раньше).
	ddNoStore := NewDrillDown(nil, nil)
	for _, data := range []string{"", "L4:1", "L2:abc", "L3:1", "L2:99999999999999999999"} {
		if res, err := ddNoStore(ctx, data); err == nil {
			t.Errorf("NewDrillDown(nil,nil)(%q) = %+v, want error", data, res)
		}
	}

	// Несуществующий digestID (L2 и L3) → error, не паника (ack-ошибка
	// диспетчера D-1 «Не получилось открыть»).
	missing := NewDrillDown(&drillStore{hasDigest: false}, staticSources())
	for _, data := range []string{"L2:7", "L3:7:0", "L3:7:3"} {
		res, err := missing(ctx, data)
		if err == nil {
			t.Errorf("%q: want error for unknown digest, got %+v", data, res)
		}
		if res != nil {
			t.Errorf("%q: result must be nil on error", data)
		}
	}

	// Ошибка снапшота источников → error (не глотаем).
	boom := NewDrillDown(
		&drillStore{hasDigest: true, digestRow: storage.Digest{ID: 1, TopicID: 2}},
		func(context.Context) ([]storage.Source, error) {
			return nil, context.DeadlineExceeded
		})
	if _, err := boom(ctx, "L2:1"); err == nil {
		t.Error("sources snapshot error: want error, got nil")
	}
}

func TestRenderDrillL2_Golden(t *testing.T) {
	ctx := context.Background()
	store := &drillStore{
		hasDigest: true,
		digestRow: storage.Digest{ID: 42, TopicID: 2, Body: "body"},
		topics:    []storage.Topic{{ID: 2, Name: "AI <новости> & ML"}},
		items: []storage.DigestItem{
			{SourceID: 3, Title: "Title A", Summary: "Первая сводка"},
			{SourceID: 4, Title: "Заголовок <2>", Summary: ""}, // пустой summary → title
			{SourceID: 3, Title: "Title B", Summary: "Вторая сводка & <b>"},
		},
	}
	dd := NewDrillDown(store, staticSources(
		storage.Source{ID: 3, Ref: "Habr <b>&</b>"},
		storage.Source{ID: 4, Ref: "arxiv.org/list/cs.AI"},
		// ID 7 ниже не встречается — срез «источник #N» в отдельном кейсе.
	))
	res, err := dd(ctx, "L2:42")
	if err != nil {
		t.Fatalf("drill L2: %v", err)
	}
	if res == nil || res.Edit == nil {
		t.Fatal("L2: want Edit, got nil")
	}
	compareGolden(t, "drill_l2_sections", res.Edit.Text)

	// Кнопки: секция 3, секция 4, затем «◀ Назад» (L1:42). Порядок секций —
	// первое появление в items, НЕ map-итерация.
	wantButtons := [][]telegram.InlineButton{
		{{Text: "Habr <b>&</b> → сообщения", Data: "L3:42:3"}},
		{{Text: "arxiv.org/list/cs.AI → сообщения", Data: "L3:42:4"}},
		{{Text: "◀ Назад", Data: "L1:42"}},
	}
	assertButtons(t, "L2 sections", res.Edit.Buttons, wantButtons)
}

func TestRenderDrillL2_UnknownSourceAndTopic(t *testing.T) {
	ctx := context.Background()
	store := &drillStore{
		hasDigest: true,
		digestRow: storage.Digest{ID: 5, TopicID: 99}, // темы 99 нет → «Тема #99»
		topics:    []storage.Topic{{ID: 2, Name: "AI"}},
		items: []storage.DigestItem{
			{SourceID: 7, Title: "T", Summary: "S"},
		},
	}
	dd := NewDrillDown(store, staticSources()) // снапшот пуст → «источник #7»
	res, err := dd(ctx, "L2:5")
	if err != nil {
		t.Fatalf("drill L2: %v", err)
	}
	compareGolden(t, "drill_l2_unknown", res.Edit.Text)
	assertButtons(t, "L2 unknown", res.Edit.Buttons, [][]telegram.InlineButton{
		{{Text: "источник #7 → сообщения", Data: "L3:5:7"}},
		{{Text: "◀ Назад", Data: "L1:5"}},
	})
}

func TestRenderDrillL2_TooManySections(t *testing.T) {
	ctx := context.Background()
	items := make([]storage.DigestItem, 0, 16)
	sources := make([]storage.Source, 0, 16)
	for i := int64(1); i <= 16; i++ {
		items = append(items, storage.DigestItem{SourceID: i, Title: "T", Summary: "S"})
		sources = append(sources, storage.Source{ID: i, Ref: "src" + strconv.FormatInt(i, 10)})
	}
	store := &drillStore{
		hasDigest: true,
		digestRow: storage.Digest{ID: 6, TopicID: 2},
		topics:    []storage.Topic{{ID: 2, Name: "Тема"}},
		items:     items,
	}
	dd := NewDrillDown(store, staticSources(sources...))
	res, err := dd(ctx, "L2:6")
	if err != nil {
		t.Fatalf("drill L2: %v", err)
	}
	compareGolden(t, "drill_l2_toomany", res.Edit.Text)
	// План §D (1333–1336): >15 секций → только «Общий список» + «◀ Назад».
	assertButtons(t, "L2 toomany", res.Edit.Buttons, [][]telegram.InlineButton{
		{{Text: "Общий список", Data: "L3:6:0"}},
		{{Text: "◀ Назад", Data: "L1:6"}},
	})
}

func TestRenderDrillL2_LongLabelTruncated(t *testing.T) {
	// Урок A2 H1: бюджет лейбла БАЙТОВЫЙ — длинное имя режется на границе
	// рун с «…», итог ≤64 байт.
	long := strings.Repeat("я", 40) // 80 байт > бюджета 38 (64−23−3)
	label := telegram.TruncateButtonLabelTo(long, l2BtnBudget) + l2BtnSuffix
	if len(label) > 64 {
		t.Fatalf("label %d bytes > 64", len(label))
	}
	if !strings.HasSuffix(label, l2BtnSuffix) || !strings.Contains(label, "…") {
		t.Fatalf("label %q: want truncation ellipsis before suffix", label)
	}
}

// Ревью D M1: секция с SourceID==0 (NULL source_id → COALESCE 0) НЕ получает
// кнопку — «L3:<did>:0» сентинел «все элементы», кнопка обещала бы фильтр.
func TestRenderDrillL2_ZeroSourceNoButton(t *testing.T) {
	res := renderDrillL2("Тема", []l2Section{
		{SourceID: 0, Name: "источник #0", Items: []storage.DigestItem{{Title: "a"}}},
		{SourceID: 7, Name: "habr", Items: []storage.DigestItem{{Title: "b"}}},
	}, 42)
	if res.Edit == nil {
		t.Fatal("nil Edit")
	}
	if !strings.Contains(res.Edit.Text, "источник #0") {
		t.Error("нулевая секция должна остаться в тексте")
	}
	for _, row := range res.Edit.Buttons {
		for _, b := range row {
			if b.Data == "L3:42:0" {
				t.Errorf("кнопка-сентинел %q для нулевой секции недопустима", b.Data)
			}
		}
	}
	want := map[string]bool{"L3:42:7": false, "L1:42": false}
	for _, row := range res.Edit.Buttons {
		for _, b := range row {
			if _, ok := want[b.Data]; ok {
				want[b.Data] = true
			}
		}
	}
	for data, seen := range want {
		if !seen {
			t.Errorf("ожидалась кнопка %q", data)
		}
	}
}

// Ревью D L7: пустой title при живой ссылке → «(без заголовка)» внутри <a>,
// а не пустая entity.
func TestRenderDrillL3_EmptyTitlePlaceholder(t *testing.T) {
	res := renderDrillL3("src", []storage.DigestItem{
		{Title: "   ", URL: "https://x.dev/a", Snippet: "s"},
		{Title: "", TgPeer: "durov", TgMsgID: 5},
	}, 42)
	if !strings.Contains(res.Edit.Text, `<a href="https://x.dev/a">(без заголовка)</a>`) {
		t.Errorf("want placeholder anchor, got:\n%s", res.Edit.Text)
	}
	if !strings.Contains(res.Edit.Text, `<a href="https://t.me/durov/5">(без заголовка)</a>`) {
		t.Errorf("want placeholder deep-link anchor, got:\n%s", res.Edit.Text)
	}
}

func TestRenderDrillL3_Golden(t *testing.T) {
	ctx := context.Background()
	longSnippet := strings.Repeat("с", 200) // 200 рун → обрезка до 120
	items := []storage.DigestItem{
		{SourceID: 3, Title: "RSS <материал>", URL: "https://example.com/a?x=1&y=2", Snippet: "Короткий snippet"},
		{SourceID: 3, Title: "JS-схема", URL: "javascript:alert(1)", TgPeer: "", TgMsgID: 0}, // без ссылки!
		{SourceID: 4, Title: "Публичный пост", TgPeer: "durov", TgMsgID: 42},
		{SourceID: 4, Title: "Приватный пост", TgPeer: "-1001234567890", TgMsgID: 7},
		{SourceID: 4, Title: "Без ссылки", URL: "", TgPeer: "", TgMsgID: 0, Snippet: ""},
		{SourceID: 5, Title: "Длинный snippet", URL: "http://plain.example/x", Snippet: longSnippet},
	}
	store := &drillStore{
		hasDigest: true,
		digestRow: storage.Digest{ID: 9, TopicID: 2},
		items:     items,
	}
	dd := NewDrillDown(store, staticSources(
		storage.Source{ID: 3, Ref: "Habr"},
		storage.Source{ID: 4, Ref: "@telegram"},
	))

	// sourceID=0 → ВСЕ элементы, заголовок «Все сообщения».
	all, err := dd(ctx, "L3:9:0")
	if err != nil {
		t.Fatalf("drill L3 all: %v", err)
	}
	compareGolden(t, "drill_l3_all", all.Edit.Text)
	assertButtons(t, "L3 all", all.Edit.Buttons, [][]telegram.InlineButton{
		{{Text: "◀ Назад", Data: "L2:9"}},
	})
	if strings.Contains(all.Edit.Text, "javascript:") {
		t.Error("javascript: URL must never reach href (план §4.8)")
	}

	// sourceID=4 → только его элементы, имя из снапшота.
	bySrc, err := dd(ctx, "L3:9:4")
	if err != nil {
		t.Fatalf("drill L3 by source: %v", err)
	}
	compareGolden(t, "drill_l3_bysource", bySrc.Edit.Text)

	// Неизвестный sourceID → «источник #77», пусто без ошибки.
	unknown, err := dd(ctx, "L3:9:77")
	if err != nil {
		t.Fatalf("drill L3 unknown source: %v", err)
	}
	compareGolden(t, "drill_l3_unknown_source", unknown.Edit.Text)
}

func TestL3Link_SchemeAndQuotes(t *testing.T) {
	tests := []struct {
		name string
		item storage.DigestItem
		want string // "" = без ссылки; иначе готовый (экранированный) href
	}{
		{"http", storage.DigestItem{URL: "http://a.b/c"}, "http://a.b/c"},
		{"https-amp", storage.DigestItem{URL: "https://a.b/?x=1&y=2"}, "https://a.b/?x=1&amp;y=2"},
		{"uppercase-scheme", storage.DigestItem{URL: "HTTPS://a.b/c"}, "HTTPS://a.b/c"},
		{"javascript", storage.DigestItem{URL: "javascript:alert(1)"}, ""},
		{"file", storage.DigestItem{URL: "file:///etc/passwd"}, ""},
		{"tg-scheme", storage.DigestItem{URL: "tg://resolve?domain=x"}, ""},
		{"quote-in-url", storage.DigestItem{URL: `https://a.b/"x`}, ""},
		{"url-fallback-deeplink", storage.DigestItem{URL: "ftp://a.b", TgPeer: "durov", TgMsgID: 5}, "https://t.me/durov/5"},
		{"peer-username", storage.DigestItem{TgPeer: "@durov", TgMsgID: 5}, "https://t.me/durov/5"},
		{"peer-private", storage.DigestItem{TgPeer: "-100123", TgMsgID: 9}, "https://t.me/c/123/9"},
		{"peer-garbage", storage.DigestItem{TgPeer: "!!", TgMsgID: 9}, ""},
		{"msg-zero", storage.DigestItem{TgPeer: "durov", TgMsgID: 0}, ""},
		{"empty", storage.DigestItem{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := l3Link(tt.item); got != tt.want {
				t.Errorf("l3Link(%+v) = %q, want %q", tt.item, got, tt.want)
			}
		})
	}
}

func TestFitDrillDownList(t *testing.T) {
	t.Run("cut-tail-with-marker", func(t *testing.T) {
		lines := make([]string, 0, 100)
		for i := 0; i < 100; i++ {
			lines = append(lines, "• "+strings.Repeat("x", 100))
		}
		got := fitDrillDownList("HEADER", lines, "\n")
		if utf8.RuneCountInString(got) > maxBodyRunes {
			t.Fatalf("got %d runes > %d", utf8.RuneCountInString(got), maxBodyRunes)
		}
		if !strings.Contains(got, "…и ещё ") {
			t.Error("want «…и ещё K» marker")
		}
		if !strings.HasPrefix(got, "HEADER\n\n• ") {
			t.Errorf("got prefix %q", got[:20])
		}
	})

	t.Run("M1-band-header-alone", func(t *testing.T) {
		// Регрессия M1 (ревью C1): заголовок ~4081–4094 рун, единственный
		// длинный блок не влезает даже с маркером → заголовок возвращается
		// КАК ЕСТЬ (≤4096), без паники и без среза.
		header := strings.Repeat("h", 4090)
		got := fitDrillDownList(header, []string{strings.Repeat("x", 100)}, "\n")
		if got != header {
			t.Fatalf("want header as-is (%d runes), got %d runes", len(header), utf8.RuneCountInString(got))
		}
	})

	t.Run("header-over-limit", func(t *testing.T) {
		header := strings.Repeat("h", 5000)
		got := fitDrillDownList(header, nil, "\n")
		if utf8.RuneCountInString(got) != maxBodyRunes || !strings.HasSuffix(got, "…") {
			t.Fatalf("want %d runes ending with …, got %d", maxBodyRunes, utf8.RuneCountInString(got))
		}
	})

	t.Run("fits-as-is", func(t *testing.T) {
		got := fitDrillDownList("H", []string{"a", "b"}, "\n")
		if got != "H\n\na\nb" {
			t.Fatalf("got %q", got)
		}
	})
}

func assertButtons(t *testing.T, label string, got, want [][]telegram.InlineButton) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d button rows, want %d: %+v", label, len(got), len(want), got)
	}
	for i := range got {
		if len(got[i]) != len(want[i]) {
			t.Fatalf("%s row %d: %d buttons, want %d", label, i, len(got[i]), len(want[i]))
		}
		for j := range got[i] {
			if got[i][j] != want[i][j] {
				t.Errorf("%s row %d col %d: got %+v, want %+v", label, i, j, got[i][j], want[i][j])
			}
		}
	}
}
