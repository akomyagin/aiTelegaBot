// Golden-тесты новых рендеров навигации (Этап 9D-1, план 9D тест-кейс 4):
// RenderTOC с overall и без (байт-в-байт регрессия), RenderTopicView (L1),
// RenderSourceView (L2), RenderMessageView (L3). compareGolden/-update —
// render_topic_test.go. Детерминизм: фиксированные срезы, goldenNow не нужен
// (новые рендеры время не используют).

package digest

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/akomyagin/aiTelegaBot/internal/storage"
	"github.com/akomyagin/aiTelegaBot/internal/telegram"
)

func TestRenderTOC_Overall_Golden(t *testing.T) {
	one := []TOCEntry{{DigestID: 1, TopicName: "Тема <1>", ItemCount: 2}}
	compareGolden(t, "toc_overall", RenderTOC(one, "15 Jan 2024", "Общее саммари & суть <выпуска>."))

	// Регрессия «ноль изменения поведения»: TOC без overall (вариативный
	// хвост пуст, "" или пробельный) байт-в-байт равен прежнему RenderTOC.
	base := RenderTOC(one, "15 Jan 2024")
	if got := RenderTOC(one, "15 Jan 2024", ""); got != base {
		t.Errorf("overall=\"\" изменил TOC:\n got %q\nwant %q", got, base)
	}
	if got := RenderTOC(one, "15 Jan 2024", "   "); got != base {
		t.Errorf("пробельный overall изменил TOC:\n got %q\nwant %q", got, base)
	}
	// Существующий toc_one.golden НЕ переписываем: байт-в-байт прежнего TOC
	// доказан сравнением выше, а golden «AI & <ML>» принадлежит
	// TestRenderTOC_Golden (render_topic_test.go).
}

func TestRenderTopicView_Golden(t *testing.T) {
	body := "📋 Тема & <1> — 15 Jan 2024\n\nHeadline темы.\n\n— 5 материалов"
	topics := []TopicEntry{
		{SourceID: 11, Name: "Канал <A>", ItemCount: 3, Summary: "Саммари & топика A."},
		{SourceID: 0, Name: "", ItemCount: 2}, // неизвестный источник: строка без саммари, БЕЗ кнопки
	}
	v := RenderTopicView(body, 7, 1700000000, topics)
	compareGolden(t, "topic_view_two", v.Text)

	// Кнопки: S:7:11 (sid=0 пропущен) + «◀ К оглавлению» TOC:<runID>.
	if len(v.Buttons) != 2 {
		t.Fatalf("Buttons = %d rows, want 2: %v", len(v.Buttons), v.Buttons)
	}
	if got := v.Buttons[0][0]; got.Text != "Канал <A>" || got.Data != "S:7:11" {
		t.Errorf("topic button = %+v", got)
	}
	if got := v.Buttons[1][0]; got.Text != "◀ К оглавлению" || got.Data != "TOC:1700000000" {
		t.Errorf("toc button = %+v", got)
	}
	if !strings.Contains(v.Text, "источник #0") {
		t.Error("fallback-имя «источник #0» отсутствует")
	}

	// legacy runID=0 → TOC-кнопки нет.
	if v0 := RenderTopicView(body, 7, 0, topics); len(v0.Buttons) != 1 {
		t.Errorf("runID=0: Buttons = %d rows, want 1", len(v0.Buttons))
	}
	// Пустой список топиков → только body (деградация).
	if ve := RenderTopicView(body, 7, 1, nil); ve.Text != body {
		t.Errorf("empty topics: Text = %q, want body %q", ve.Text, body)
	}
}

func TestRenderTopicView_LongNameLabelBytes(t *testing.T) {
	long := strings.Repeat("ы", 100) // 200 байт — вдвое больше лимита Bot API
	v := RenderTopicView("body", 7, 1, []TopicEntry{{SourceID: 12, Name: long, ItemCount: 1, Summary: "s"}})
	label := v.Buttons[0][0].Text
	if len(label) > 64 {
		t.Errorf("лейбл = %d БАЙТ, want ≤64", len(label))
	}
	if !strings.HasSuffix(label, "…") {
		t.Errorf("лейбл %q без «…»", label)
	}
	if !strings.Contains(v.Text, long) {
		t.Error("полное имя не осталось в тексте L1")
	}
	compareGolden(t, "topic_view_longname", v.Text)
}

func TestRenderTopicView_Guard4096(t *testing.T) {
	topics := make([]TopicEntry, 0, 200)
	for i := range 200 {
		topics = append(topics, TopicEntry{
			SourceID: int64(i + 1), Name: "Канал " + strconv.Itoa(i),
			ItemCount: 9, Summary: strings.Repeat("саммари ", 20),
		})
	}
	v := RenderTopicView("📋 Тема — 15 Jan 2024", 7, 1, topics)
	if n := utf8.RuneCountInString(v.Text); n > 4096 {
		t.Errorf("Text = %d рун, want ≤4096", n)
	}
	if !strings.Contains(v.Text, "…и ещё ") {
		t.Error("нет маркера «…и ещё K» (блоками-топиками)")
	}
	compareGolden(t, "topic_view_over4096", v.Text)
}

func navItem(id int64, title, url, summary string) storage.DigestItem {
	return storage.DigestItem{ID: id, SourceID: 5, Title: title, URL: url, Summary: summary}
}

func TestRenderSourceView_Golden(t *testing.T) {
	sections := []SourceSection{{
		SourceID: 5, Name: "Канал <A>", Summary: "Саммари топика.",
		Items: []storage.DigestItem{
			navItem(101, "Новость & <1>", "https://example.com/1", "Саммари первой.\n↳ Статья: суть."),
			{ID: 102, SourceID: 5, Title: "Без ссылки"}, // пустой summary → title, нет LINK → нет <a>/«перейти»
			{ID: 103, SourceID: 5, Title: "Курс-пост", URL: "https://example.com/course", Summary: "Курс: Go — основы", Class: "course"},
			{ID: 104, SourceID: 5, Title: "С фото", URL: "https://example.com/p", Summary: "Фото-пост", MediaKind: "photo", MediaRef: "AQACfileid"},
		},
	}}
	v := RenderSourceView("Тема & <1>", 7, 5, sections)
	compareGolden(t, "source_view_normal", v.Text)

	// Шапка: «📰 <тема> / <имя топика>» + саммари топика.
	if !strings.HasPrefix(v.Text, "📰 Тема &amp; &lt;1&gt; / Канал &lt;A&gt;\n\nСаммари топика.") {
		t.Errorf("шапка = %q", v.Text[:min(len(v.Text), 120)])
	}
	if !strings.Contains(v.Text, "🎓") {
		t.Error("course-маркер отсутствует")
	}
	if !strings.Contains(v.Text, "🖼") {
		t.Error("media-маркер (заготовка 9E) отсутствует")
	}

	// Кнопки: «Читать ▸» M: на не-course строку + «◀ Назад» T:<did>;
	// у course-строки кнопки НЕТ (план 9D).
	wantRows := []struct{ text, data string }{
		{"Читать ▸", "M:101"}, {"Читать ▸", "M:102"}, {"Читать ▸", "M:104"}, {"◀ Назад", "T:7"},
	}
	if len(v.Buttons) != len(wantRows) {
		t.Fatalf("Buttons = %d rows, want %d: %v", len(v.Buttons), len(wantRows), v.Buttons)
	}
	for i, w := range wantRows {
		if got := v.Buttons[i][0]; got.Text != w.text || got.Data != w.data {
			t.Errorf("Buttons[%d] = %+v, want {%s %s}", i, got, w.text, w.data)
		}
	}
	if len(v.URLButtons) != 0 {
		t.Errorf("L2 URLButtons = %+v, want none", v.URLButtons)
	}
}

func TestRenderSourceView_AllMode(t *testing.T) {
	sections := []SourceSection{
		{SourceID: 5, Name: "Канал A", Summary: "Саммари A.", Items: []storage.DigestItem{navItem(1, "T1", "https://a/1", "s1")}},
		{SourceID: 6, Name: "RSS B", Items: []storage.DigestItem{{ID: 2, SourceID: 6, Title: "T2", Summary: "s2"}}},
	}
	v := RenderSourceView("Тема", 7, 0, sections)
	compareGolden(t, "source_view_all", v.Text)
	if !strings.Contains(v.Text, "<b>Канал A</b>\nСаммари A.") || !strings.Contains(v.Text, "<b>RSS B</b>") {
		t.Errorf("подзаголовки секций: %q", v.Text)
	}
	if len(v.Buttons) != 3 { // M:1, M:2, T:7
		t.Fatalf("Buttons = %d rows, want 3", len(v.Buttons))
	}
}

func TestRenderSourceView_Over20(t *testing.T) {
	items := make([]storage.DigestItem, 0, 25)
	for i := range 25 {
		items = append(items, navItem(int64(i+1), "T"+strconv.Itoa(i), "https://a/"+strconv.Itoa(i), "s"+strconv.Itoa(i)))
	}
	v := RenderSourceView("Тема", 7, 5, []SourceSection{{SourceID: 5, Name: "A", Items: items}})
	if !strings.Contains(v.Text, "…и ещё 5") {
		t.Error("нет «…и ещё 5» (первые 20 из 25)")
	}
	if n := utf8.RuneCountInString(v.Text); n > 4096 {
		t.Errorf("Text = %d рун, want ≤4096", n)
	}
	if len(v.Buttons) != 21 { // 20 «Читать ▸» + «◀ Назад»
		t.Fatalf("Buttons = %d rows, want 21", len(v.Buttons))
	}
	if v.Buttons[19][0].Data != "M:20" || v.Buttons[20][0].Data != "T:7" {
		t.Errorf("хвост кнопок: %v", v.Buttons[19:])
	}
	compareGolden(t, "source_view_over20", v.Text)
}

func TestRenderSourceView_Empty(t *testing.T) {
	v := RenderSourceView("Тема", 7, 5, []SourceSection{{SourceID: 5, Name: "A"}})
	if !strings.Contains(v.Text, "Материалов нет") {
		t.Errorf("Text = %q", v.Text)
	}
	if len(v.Buttons) != 1 || v.Buttons[0][0].Data != "T:7" || v.Buttons[0][0].Text != "◀ Назад" {
		t.Errorf("Buttons = %v, want [◀ Назад T:7]", v.Buttons)
	}
	compareGolden(t, "source_view_empty", v.Text)
}

func TestRenderMessageView_Golden(t *testing.T) {
	it := storage.DigestItem{
		ID: 101, DigestID: 7, SourceID: 5,
		Title:     "Заголовок & <сообщения>",
		URL:       "https://example.com/post?a=1&b=2",
		FullText:  "Полный текст сообщения.\nВторая строка.",
		MediaKind: "photo", MediaRef: "AQACfileid",
	}
	links := []MessageLink{
		{URL: "https://article.example/x", Kind: "article", Title: "Статья <N>", Summary: "Суть & статьи."},
		{URL: `https://bad.example/"quoted"`, Kind: "other", Title: "Кавычка"}, // отвергнута (кавычка)
		{URL: "ftp://files.example/y", Kind: "other", Title: "FTP"},            // отвергнута (не http/https)
		{URL: "https://notitle.example/z", Kind: "course"},                     // домен как лейбл + 🎓
	}
	v := RenderMessageView(it, links)
	compareGolden(t, "message_view_full", v.Text)

	if strings.Contains(v.Text, "bad.example") || strings.Contains(v.Text, "ftp://") {
		t.Error("отвергнутые URL попали в текст")
	}
	if !strings.Contains(v.Text, "📄 Статья &lt;N&gt;</a> — Суть &amp; статьи.") {
		t.Error("строка ссылки article не в golden-формате")
	}
	if !strings.Contains(v.Text, "🎓 notitle.example</a>") {
		t.Error("домен-fallback для пустого title отсутствует")
	}
	if strings.Contains(v.Text, "legacy") {
		t.Error("full_text жив — legacy-пометки быть не должно")
	}

	// Кнопки (план §4.6): [🖼 Фото P:101], nil-плейсхолдер URL-кнопки,
	// [◀ Назад S:7:5]; URLButtons — СЫРОЙ URL (без HTML-экранирования).
	if len(v.Buttons) != 3 || v.Buttons[1] != nil {
		t.Fatalf("Buttons = %v", v.Buttons)
	}
	if got := v.Buttons[0][0]; got.Text != "🖼 Фото" || got.Data != "P:101" {
		t.Errorf("photo button = %+v", got)
	}
	if got := v.Buttons[2][0]; got.Text != "◀ Назад" || got.Data != "S:7:5" {
		t.Errorf("back button = %+v", got)
	}
	want := URLButton{Text: "↗ Перейти в источник", URL: "https://example.com/post?a=1&b=2"}
	if len(v.URLButtons) != 1 || v.URLButtons[0] != want {
		t.Errorf("URLButtons = %+v, want [%+v]", v.URLButtons, want)
	}
}

func TestRenderMessageView_Legacy(t *testing.T) {
	it := storage.DigestItem{ID: 5, DigestID: 7, Title: "T", Snippet: "Только фрагмент текста."}
	v := RenderMessageView(it, nil)
	if !strings.Contains(v.Text, "(сохранён только фрагмент — legacy)") {
		t.Errorf("нет legacy-пометки: %q", v.Text)
	}
	if !strings.Contains(v.Text, "Только фрагмент текста.") {
		t.Errorf("snippet не отрендерен: %q", v.Text)
	}
	// Нет URL и нет tg deep link → URL-кнопки нет; SourceID=0 → «◀ Назад» S:7:0.
	if len(v.URLButtons) != 0 {
		t.Errorf("URLButtons = %+v, want none", v.URLButtons)
	}
	if len(v.Buttons) != 1 || v.Buttons[0][0].Data != "S:7:0" {
		t.Errorf("Buttons = %v, want [S:7:0]", v.Buttons)
	}
	compareGolden(t, "message_view_legacy", v.Text)
}

func TestRenderMessageView_LongTextCut(t *testing.T) {
	it := storage.DigestItem{
		ID: 9, DigestID: 7, SourceID: 5, Title: "T",
		URL:      "https://example.com/long",
		FullText: strings.Repeat("длинное слово ", 1000), // ~14000 рун
	}
	links := []MessageLink{{URL: "https://l.example/1", Kind: "article", Title: "L1", Summary: "S"}}
	v := RenderMessageView(it, links)
	if n := utf8.RuneCountInString(v.Text); n > 4096 {
		t.Errorf("Text = %d рун, want ≤4096", n)
	}
	if !strings.Contains(v.Text, "…полный текст — в источнике") {
		t.Error("нет пометки резки full_text")
	}
	// URL-кнопка жива даже при резке текста.
	if len(v.URLButtons) != 1 || v.URLButtons[0].URL != "https://example.com/long" {
		t.Errorf("URLButtons = %+v", v.URLButtons)
	}
	compareGolden(t, "message_view_long", v.Text)
}

// Защита контракта NavView для 9D-2: число nil-плейсхолдеров в Buttons равно
// len(URLButtons) — во всех трёх рендерах.
func TestNavViewPlaceholderInvariant(t *testing.T) {
	views := []*NavView{
		RenderTopicView("b", 7, 1, []TopicEntry{{SourceID: 5, Name: "A", ItemCount: 1}}),
		RenderSourceView("t", 7, 5, []SourceSection{{SourceID: 5, Name: "A", Items: []storage.DigestItem{navItem(1, "T", "https://a/1", "s")}}}),
		RenderMessageView(storage.DigestItem{ID: 1, DigestID: 7, SourceID: 5, Title: "T", URL: "https://a/1", FullText: "f"}, nil),
	}
	for i, v := range views {
		nils := 0
		for _, row := range v.Buttons {
			if row == nil {
				nils++
				continue
			}
			for _, b := range row {
				if len(b.Text) > 64 {
					t.Errorf("view %d: лейбл %q = %d байт (>64)", i, b.Text, len(b.Text))
				}
				if b.Data == "" {
					t.Errorf("view %d: Data-кнопка без data: %+v", i, b)
				}
			}
		}
		if nils != len(v.URLButtons) {
			t.Errorf("view %d: %d nil-строк, %d URLButtons — контракт нарушен", i, nils, len(v.URLButtons))
		}
	}
	_ = telegram.InlineButton{} // компиляционная связка: Buttons — тип telegram
}

// Ревью 9D M1: длинный одностирочный title (tg firstLine — до ~4096 рун) не
// должен выводить текст L3 за бюджет 4096 → MESSAGE_TOO_LONG.
func TestRenderMessageView_LongSingleLineTitle(t *testing.T) {
	it := storage.DigestItem{
		ID: 1, DigestID: 7, SourceID: 5,
		Title:    strings.Repeat("я", 4000),
		FullText: "Тело сообщения.",
	}
	v := RenderMessageView(it, nil)
	if n := utf8.RuneCountInString(v.Text); n > 4096 {
		t.Errorf("Text = %d рун, want ≤4096", n)
	}
	if !strings.Contains(v.Text, "<b>"+strings.Repeat("я", navHeaderTitleRunes)+"…</b>") {
		t.Errorf("шапка без усечённого до %d рун title с «…»: %.120q", navHeaderTitleRunes, v.Text)
	}
	if strings.Contains(v.Text, strings.Repeat("я", navHeaderTitleRunes+1)) {
		t.Error("title НЕ усечён — в тексте больше рун, чем navHeaderTitleRunes")
	}
}

// Ревью 9D L1: сверхдлинный title блока L2 усекается, а блок остаётся ВИДИМ
// (раньше молча отбрасывался guard'ом fitDrillDownList).
func TestRenderSourceView_LongTitleBlockVisible(t *testing.T) {
	long := strings.Repeat("я", 4000)
	v := RenderSourceView("Тема", 7, 5, []SourceSection{{SourceID: 5, Name: "A",
		Items: []storage.DigestItem{navItem(1, long, "https://a/1", "s1")}}})
	want := `<a href="https://a/1">` + strings.Repeat("я", navBlockTitleRunes) + `…</a>`
	if !strings.Contains(v.Text, want) {
		t.Errorf("блок с усечённым title не виден в тексте: %.200q", v.Text)
	}
	if strings.Contains(v.Text, strings.Repeat("я", navBlockTitleRunes+1)) {
		t.Error("title НЕ усечён — в тексте больше рун, чем navBlockTitleRunes")
	}
	if !strings.Contains(v.Text, "перейти</a>") || !strings.Contains(v.Text, "s1") {
		t.Error("хвост блока (перейти/саммари) отсутствует — блок отброшен guard'ом")
	}
	if n := utf8.RuneCountInString(v.Text); n > 4096 {
		t.Errorf("Text = %d рун, want ≤4096", n)
	}
}

// Ревью 9D L6: truncation ровно на границе секций (sid=0) — подзаголовок
// следующей секции НЕ рендерится без показанных элементов.
func TestRenderSourceView_AllMode_TruncateAtSectionBoundary(t *testing.T) {
	mk := func(base, n int) []storage.DigestItem {
		items := make([]storage.DigestItem, 0, n)
		for i := range n {
			items = append(items, navItem(int64(base+i), "T"+strconv.Itoa(base+i),
				"https://a/"+strconv.Itoa(base+i), "s"))
		}
		return items
	}
	sections := []SourceSection{
		{SourceID: 5, Name: "A", Items: mk(1, sourceViewMaxItems)}, // ровно 20
		{SourceID: 6, Name: "B", Items: mk(100, 5)},                // граница — перед B
	}
	v := RenderSourceView("Тема", 7, 0, sections)
	if !strings.Contains(v.Text, "<b>A</b>") {
		t.Error("подзаголовок показанной секции A отсутствует")
	}
	if strings.Contains(v.Text, "<b>B</b>") {
		t.Error("подзаголовок секции B отрендерен без единого показанного элемента (ревью 9D L6)")
	}
	if !strings.Contains(v.Text, "…и ещё 5") {
		t.Error("нет маркера «…и ещё 5»")
	}
}
