// Тесты 9D-2a (план §4.6, «Подэтап 9D» тест-кейсы 5 и 7): диспетчер
// S:/M:/P:/PDEL + legacy-alias'ы L2:/L3:, парсеры (мусор/не-числа/overflow →
// error, не паника), бюджет callback_data ≤64 байт на 19-значных id,
// sections_json (деградация на битом JSON), peer-Title fallback имён,
// M:-view (GetDigestItem miss → error, блок «Ссылки:»), P: (9E: item miss →
// error; sendPhoto-кейсы — photo_caption_test.go), PDEL-защита прямых
// вызовов, крайний случай «digest без items».

package digest

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/akomyagin/aiTelegaBot/internal/storage"
	"github.com/akomyagin/aiTelegaBot/internal/telegram"
)

// drillStore — фейк storage.Store для диспетчера 9D: используются только
// GetDigest/ListDigestItems/ListDigestItemsBySource/ListTopics/GetDigestItem/
// ListItemLinks/GetLinkCache/GetPeer, остальное паникует через nil-embedding
// (осознанно: seam их вызывать не должен).
type drillStore struct {
	storage.Store
	digestRow storage.Digest
	hasDigest bool
	items     []storage.DigestItem
	topics    []storage.Topic
	item      storage.DigestItem
	hasItem   bool
	links     []storage.ItemLink
	cache     map[string]storage.LinkCacheRow
	peers     map[int64]storage.Peer
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

func (s *drillStore) GetDigestItem(ctx context.Context, itemID int64) (storage.DigestItem, bool, error) {
	return s.item, s.hasItem, nil
}

func (s *drillStore) ListItemLinks(ctx context.Context, dedupKey string) ([]storage.ItemLink, error) {
	return s.links, nil
}

func (s *drillStore) GetLinkCache(ctx context.Context, urls []string) (map[string]storage.LinkCacheRow, error) {
	if s.cache == nil {
		return map[string]storage.LinkCacheRow{}, nil
	}
	return s.cache, nil
}

func (s *drillStore) GetPeer(ctx context.Context, peerID int64) (storage.Peer, bool, error) {
	p, ok := s.peers[peerID]
	return p, ok, nil
}

func staticSources(rows ...storage.Source) func(context.Context) ([]storage.Source, error) {
	return func(context.Context) ([]storage.Source, error) { return rows, nil }
}

// --- парсеры (тест-кейс 5 плана) ---

func TestParseSourceData(t *testing.T) {
	valid := []struct {
		data   string
		prefix string
		did    int64
		sid    int64
	}{
		{"S:42:7", "S:", 42, 7},
		{"S:42:0", "S:", 42, 0},
		{"L3:42:7", "L3:", 42, 7},
		{"L3:42:0", "L3:", 42, 0},
		{"S:9223372036854775807:9223372036854775807", "S:", 9223372036854775807, 9223372036854775807},
	}
	for _, tt := range valid {
		t.Run("ok/"+tt.data, func(t *testing.T) {
			did, sid, err := parseSourceData(tt.data, tt.prefix)
			if err != nil {
				t.Fatalf("parseSourceData(%q): %v", tt.data, err)
			}
			if did != tt.did || sid != tt.sid {
				t.Errorf("parseSourceData(%q) = (%d,%d), want (%d,%d)", tt.data, did, sid, tt.did, tt.sid)
			}
		})
	}

	bad := []string{
		"", "S:", "S:1", "S:1:", "S:abc:1", "S:1:x", "S:0:1", "S:-1:1", "S:1:-2",
		"S:1:2:3", "S::1", "S: 1:2", "S:99999999999999999999:1", "S:1:99999999999999999999",
		"L3:1", "L3:1:x", "L3:1:-2", "L3:99999999999999999999:1", "T:42",
	}
	for _, data := range bad {
		t.Run("err/"+data, func(t *testing.T) {
			prefix := data
			if i := strings.Index(data, ":"); i >= 0 {
				prefix = data[:i+1]
			}
			if _, _, err := parseSourceData(data, prefix); err == nil {
				t.Fatalf("parseSourceData(%q): want error, got nil", data)
			}
		})
	}
}

func TestParseMessageData(t *testing.T) {
	for _, tt := range []struct {
		data   string
		prefix string
		want   int64
	}{
		{"M:42", "M:", 42},
		{"P:1", "P:", 1},
		{"L2:9223372036854775807", "L2:", 9223372036854775807},
	} {
		got, err := parseMessageData(tt.data, tt.prefix)
		if err != nil || got != tt.want {
			t.Errorf("parseMessageData(%q) = (%d,%v), want (%d,nil)", tt.data, got, err, tt.want)
		}
	}
	for _, data := range []string{
		"M:", "M:abc", "M:0", "M:-1", "M:1:2", "M:99999999999999999999",
		"L2:", "L2:abc", "L2:99999999999999999999", "P:x",
	} {
		prefix := data
		if i := strings.Index(data, ":"); i >= 0 {
			prefix = data[:i+1]
		}
		if _, err := parseMessageData(data, prefix); err == nil {
			t.Errorf("parseMessageData(%q): want error, got nil", data)
		}
	}
}

// Property-тест бюджетов (тест-кейс 5 плана): все префиксы навигации с
// 19-значными id (worst case int64) укладываются в 64 БАЙТА callback_data.
func TestCallbackDataBudget9D(t *testing.T) {
	const big = "9223372036854775807" // 19 знаков = math.MaxInt64
	for _, d := range []string{
		"S:" + big + ":" + big,  // 41
		"M:" + big,              // 21
		"P:" + big,              // 21
		"PDEL",                  // 4 — константа
		"T:" + big,              // 21
		"TOC:" + big,            // 23
		"L1:" + big,             // 22
		"L2:" + big,             // 22
		"L3:" + big + ":" + big, // 42
	} {
		if len(d) > 64 {
			t.Errorf("callback_data %q = %d байт > 64", d, len(d))
		}
	}
}

// --- диспетчер (тест-кейсы 5, 7 плана) ---

func TestNewDrillDown_Dispatch9D(t *testing.T) {
	ctx := context.Background()
	store := &drillStore{
		hasDigest: true,
		digestRow: storage.Digest{ID: 42, RunID: 500, TopicID: 2, Body: "<b>Шапка темы</b>",
			SectionsJSON: `[{"source_id":3,"summary":"Саммари топика C"}]`},
		topics: []storage.Topic{{ID: 2, Name: "AI"}},
		items: []storage.DigestItem{
			{ID: 11, SourceID: 3, DedupKey: "k1", Title: "Title A", Summary: "Первая сводка", URL: "https://a.example/1"},
			{ID: 12, SourceID: 4, Title: "Title B", Summary: "Вторая"},
			{ID: 13, SourceID: 3, DedupKey: "k3", Title: "Title C", Summary: "Третья"},
		},
		peers: map[int64]storage.Peer{1234567890: {MTProtoID: 1234567890, Title: "Приватный канал"}},
	}
	dd := NewDrillDown(store, staticSources(
		storage.Source{ID: 3, Kind: "rss", Ref: "Habr <b>&</b>"},
		storage.Source{ID: 4, Kind: "tg_mtproto", Ref: "-1001234567890"},
	))

	// Legacy «L2:<did>» → как «T:<did>»: RenderTopicView — body + «Топики:» +
	// саммари из sections_json + peer-Title fallback + кнопки S:/TOC:.
	res, err := dd(ctx, "L2:42")
	if err != nil || res == nil || res.Edit == nil {
		t.Fatalf("L2:42 = %+v, %v", res, err)
	}
	for _, want := range []string{"<b>Шапка темы</b>", "Топики:", "Саммари топика C",
		"Habr &lt;b&gt;&amp;&lt;/b&gt;", "Приватный канал", "2 сообщ."} {
		if !strings.Contains(res.Edit.Text, want) {
			t.Errorf("L2 text lacks %q:\n%s", want, res.Edit.Text)
		}
	}
	assertButtons(t, "L2→TopicView", res.Edit.Buttons, [][]telegram.InlineButton{
		{{Text: "Habr <b>&</b>", Data: "S:42:3"}},
		{{Text: "Приватный канал", Data: "S:42:4"}},
		{{Text: "◀ К оглавлению", Data: "TOC:500"}},
	})

	// Legacy «L3:<did>:<sid>» → S:-view: шапка «📰 AI / <имя>» + саммари топика.
	res, err = dd(ctx, "L3:42:3")
	if err != nil || res == nil || res.Edit == nil {
		t.Fatalf("L3:42:3 = %+v, %v", res, err)
	}
	for _, want := range []string{"📰 AI / Habr &lt;b&gt;&amp;&lt;/b&gt;", "Саммари топика C", "Первая сводка"} {
		if !strings.Contains(res.Edit.Text, want) {
			t.Errorf("L3 text lacks %q:\n%s", want, res.Edit.Text)
		}
	}
	assertButtons(t, "L3→SourceView", res.Edit.Buttons, [][]telegram.InlineButton{
		{{Text: "Читать ▸", Data: "M:11"}},
		{{Text: "Читать ▸", Data: "M:13"}},
		{{Text: "◀ Назад", Data: "T:42"}},
	})

	// «S:<did>:0» → все топики с подзаголовками.
	res, err = dd(ctx, "S:42:0")
	if err != nil || res == nil || res.Edit == nil {
		t.Fatalf("S:42:0 = %+v, %v", res, err)
	}
	for _, want := range []string{"<b>Habr &lt;b&gt;&amp;&lt;/b&gt;</b>", "<b>Приватный канал</b>", "Вторая"} {
		if !strings.Contains(res.Edit.Text, want) {
			t.Errorf("S:42:0 text lacks %q:\n%s", want, res.Edit.Text)
		}
	}
	if len(res.Edit.Buttons) != 4 { // M:11, M:12, M:13, «◀ Назад»
		t.Errorf("S:42:0 buttons = %+v, want 4 rows", res.Edit.Buttons)
	}

	// «P:» (9E): GetDigestItem miss (hasItem=false у этого store) → error —
	// ack «Не получилось открыть» на стороне telegram. Полноценные
	// sendPhoto-кейсы (file_id/URL/caption/golden) — photo_caption_test.go.
	if res, err := dd(ctx, "P:11"); err == nil || res != nil {
		t.Errorf("P:11 (item miss) = %+v, %v, want error/nil", res, err)
	}
	// «PDEL» → ack-only защита прямых вызовов seam'а (в проде telegram
	// перехватывает PDEL раньше — handlePhotoDelete, Этап 9E).
	res, err = dd(ctx, "PDEL")
	if err != nil || res == nil || res.AckText != "" || res.Edit != nil || res.Send != nil {
		t.Errorf("PDEL = %+v, %v, want empty ack-only", res, err)
	}
	// Неизвестный префикс и мусор → error, не паника.
	for _, data := range []string{"", "X:1", "S:abc:1", "S:1", "M:abc", "M:99999999999999999999",
		"L2:", "L3:1", "S:99999999999999999999:1", "P:garbage"} {
		if res, err := dd(ctx, data); err == nil {
			t.Errorf("%q: want error, got %+v", data, res)
		}
	}
}

func TestNewDrillDown_MessageView(t *testing.T) {
	ctx := context.Background()
	store := &drillStore{
		hasItem: true,
		item: storage.DigestItem{ID: 11, DigestID: 42, SourceID: 3, DedupKey: "dk1",
			Title: "Заголовок", FullText: "Полный текст", URL: "https://a.example/1",
			MediaRef: "AgADBAAD", MediaKind: "photo"},
		links: []storage.ItemLink{{DedupKey: "dk1", URL: "https://article.example/x", Kind: "article", Ord: 0}},
		cache: map[string]storage.LinkCacheRow{
			"https://article.example/x": {URL: "https://article.example/x", Kind: "article", Title: "Статья", Summary: "Саммари статьи"},
		},
	}
	dd := NewDrillDown(store, nil)
	res, err := dd(ctx, "M:11")
	if err != nil || res == nil || res.Edit == nil {
		t.Fatalf("M:11 = %+v, %v", res, err)
	}
	for _, want := range []string{"<b>Заголовок</b>", "Полный текст", "Ссылки:", "📄 Статья", "— Саммари статьи"} {
		if !strings.Contains(res.Edit.Text, want) {
			t.Errorf("M: text lacks %q:\n%s", want, res.Edit.Text)
		}
	}
	// Кнопки (§4.6): «🖼 Фото» P: → URL-кнопка «↗ Перейти в источник» →
	// «◀ Назад» S:<did>:<sid> (did/sid из строки digest_items).
	if len(res.Edit.Buttons) != 3 {
		t.Fatalf("M: buttons = %+v, want 3 rows", res.Edit.Buttons)
	}
	if res.Edit.Buttons[0][0].Data != "P:11" || res.Edit.Buttons[0][0].Text != "🖼 Фото" {
		t.Errorf("row0 = %+v, want 🖼 Фото P:11", res.Edit.Buttons[0])
	}
	url := res.Edit.Buttons[1][0]
	if url.Text != "↗ Перейти в источник" || url.URL != "https://a.example/1" || url.Data != "" {
		t.Errorf("row1 = %+v, want URL-кнопку ↗", url)
	}
	if res.Edit.Buttons[2][0].Data != "S:42:3" {
		t.Errorf("row2 = %+v, want ◀ Назад S:42:3", res.Edit.Buttons[2])
	}

	// Отсутствующий id → error (ack «Не получилось открыть»), не паника.
	miss := NewDrillDown(&drillStore{hasItem: false}, nil)
	if res, err := miss(ctx, "M:777"); err == nil || res != nil {
		t.Errorf("M:777 = %+v, %v, want error/nil", res, err)
	}
}

func TestNewDrillDown_Errors(t *testing.T) {
	ctx := context.Background()
	// Мусорные data → error даже без обращения к store (nil-store не паникует:
	// парсер отказывает раньше). PDEL — единственное, что работает без store.
	ddNoStore := NewDrillDown(nil, nil)
	for _, data := range []string{"", "X:1", "S:abc:1", "M:abc", "L2:99999999999999999999", "L3:1"} {
		if res, err := ddNoStore(ctx, data); err == nil {
			t.Errorf("NewDrillDown(nil,nil)(%q) = %+v, want error", data, res)
		}
	}
	if res, err := ddNoStore(ctx, "PDEL"); err != nil || res == nil || res.Edit != nil {
		t.Errorf("PDEL без store = %+v, %v, want ack-only", res, err)
	}

	// Несуществующий digestID (S: и legacy L2:/L3:) → error, не паника.
	missing := NewDrillDown(&drillStore{hasDigest: false}, staticSources())
	for _, data := range []string{"S:7:0", "S:7:3", "L2:7", "L3:7:0"} {
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
		func(context.Context) ([]storage.Source, error) { return nil, context.DeadlineExceeded })
	for _, data := range []string{"L2:1", "S:1:0"} {
		if _, err := boom(ctx, data); err == nil {
			t.Errorf("%q: sources snapshot error: want error, got nil", data)
		}
	}

	// Крайний случай §4.6: digest без items → «Материалов нет» + «◀ Назад».
	empty := NewDrillDown(&drillStore{
		hasDigest: true,
		digestRow: storage.Digest{ID: 8, TopicID: 2},
		topics:    []storage.Topic{{ID: 2, Name: "Тема"}},
	}, staticSources())
	res, err := empty(ctx, "S:8:0")
	if err != nil || res == nil || res.Edit == nil {
		t.Fatalf("S:8:0 = %+v, %v", res, err)
	}
	if !strings.Contains(res.Edit.Text, "Материалов нет") {
		t.Errorf("empty digest text = %q, want «Материалов нет»", res.Edit.Text)
	}
	assertButtons(t, "empty S:", res.Edit.Buttons, [][]telegram.InlineButton{
		{{Text: "◀ Назад", Data: "T:8"}},
	})
}

// --- sections_json и имена источников ---

func TestParseTopicSections(t *testing.T) {
	got := ParseTopicSections(`[{"source_id":3,"summary":"s3"},{"source_id":4,"summary":"s4"},{"source_id":3,"summary":"dup"},{"source_id":5,"summary":"  "},{"source_id":6,"summary":"s6"}]`)
	want := map[int64]string{3: "s3", 4: "s4", 6: "s6"}
	if len(got) != len(want) {
		t.Fatalf("sections = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("sections[%d] = %q, want %q", k, got[k], v)
		}
	}
	// Пусто/битый JSON → пустая map (деградация, не ошибка).
	for _, s := range []string{"", "   ", "не json", `[{"source_id":1`, `{"source_id":1}`} {
		if m := ParseTopicSections(s); len(m) != 0 {
			t.Errorf("ParseTopicSections(%q) = %v, want empty", s, m)
		}
	}
}

func TestSourceDisplayNames_PeerFallback(t *testing.T) {
	ctx := context.Background()
	store := &drillStore{peers: map[int64]storage.Peer{
		123: {MTProtoID: 123, Title: "Название канала"},
		456: {MTProtoID: 456, Title: "   "}, // пустой Title → Ref
	}}
	names, err := SourceDisplayNames(ctx, store, staticSources(
		storage.Source{ID: 1, Kind: "tg_mtproto", Ref: "-100123"},
		storage.Source{ID: 2, Kind: "tg_mtproto", Ref: "-100456"},
		storage.Source{ID: 3, Kind: "tg_mtproto", Ref: "-100999"}, // нет peer-строки → Ref
		storage.Source{ID: 4, Kind: "rss", Ref: "-100123"},        // не tg_* → Ref как есть
		storage.Source{ID: 5, Kind: "tg_public", Ref: "durov"},    // username → Ref (GetPeer keyed by id)
	))
	if err != nil {
		t.Fatalf("SourceDisplayNames: %v", err)
	}
	want := map[int64]string{1: "Название канала", 2: "-100456", 3: "-100999", 4: "-100123", 5: "durov"}
	for id, w := range want {
		if names[id] != w {
			t.Errorf("names[%d] = %q, want %q", id, names[id], w)
		}
	}
	// sources == nil → пустой снапшот (деградация «источник #N» в рендерах).
	if m, err := SourceDisplayNames(ctx, store, nil); err != nil || len(m) != 0 {
		t.Errorf("nil sources = %v, %v, want empty", m, err)
	}
	// Ошибка снапшота → error.
	if _, err := SourceDisplayNames(ctx, store, func(context.Context) ([]storage.Source, error) {
		return nil, context.DeadlineExceeded
	}); err == nil {
		t.Error("snapshot error: want error, got nil")
	}
}

// --- сохранённые тесты живых хелперов (l3Link, fitDrillDownList) ---

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
		got := fitDrillDownList(strings.Repeat("z", 5000), nil, "\n")
		if n := utf8.RuneCountInString(got); n != maxBodyRunes {
			t.Errorf("rune count = %d, want exactly %d", n, maxBodyRunes)
		}
		if !strings.HasSuffix(got, "…") {
			t.Error("header cut must end with …")
		}
	})
}

// assertButtons — побайтовое сравнение клавиатуры (образец из старых
// drilldown-тестов Этапа 8 D).
func assertButtons(t *testing.T, label string, got, want [][]telegram.InlineButton) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d rows, want %d: %+v", label, len(got), len(want), got)
	}
	for i := range want {
		if len(got[i]) != len(want[i]) {
			t.Fatalf("%s row %d: %+v, want %+v", label, i, got[i], want[i])
		}
		for j := range want[i] {
			if got[i][j] != want[i][j] {
				t.Errorf("%s row %d btn %d: %+v, want %+v", label, i, j, got[i][j], want[i][j])
			}
		}
	}
}
