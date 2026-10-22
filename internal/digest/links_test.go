// Тесты extractURLs — тест-кейс 2 плана Этапа 9 (подэтап 9B): извлечение,
// нормализация, дедуп, порядок появления, обрезка по max, t.me-фильтр.
// Table-driven, без сети (SKILL.md §7).

package digest

import (
	"reflect"
	"testing"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
)

func TestExtractURLs(t *testing.T) {
	tests := []struct {
		name string
		item feed.Item
		max  int
		want []string
	}{
		{
			name: "http и https из Text, порядок появления",
			item: feed.Item{Text: "see http://a.example/one and https://b.example/two"},
			max:  5,
			want: []string{"http://a.example/one", "https://b.example/two"},
		},
		{
			name: "Item.URL первым, затем Text",
			item: feed.Item{URL: "https://post.example/news", Text: "details https://ref.example/x"},
			max:  5,
			want: []string{"https://post.example/news", "https://ref.example/x"},
		},
		{
			name: "Item.URL дублирует Text — одна запись, Item.URL первый кандидат",
			item: feed.Item{URL: "https://post.example/news", Text: "same https://post.example/news again"},
			max:  5,
			want: []string{"https://post.example/news"},
		},
		{
			name: "дубли нормализуются: HOST регистр, trailing slash, fragment",
			item: feed.Item{Text: "https://EXAMPLE.com/Path/ http://example.com/path#frag https://example.com/Path/?q=1#/f"},
			max:  5,
			// /Path/ и /path — РАЗНЫЕ (path case-sensitive); /Path/?q=1 тоже.
			want: []string{"https://example.com/Path", "http://example.com/path", "https://example.com/Path?q=1"},
		},
		{
			name: "t.me и telegram.me с поддоменами НЕ извлекаются",
			item: feed.Item{Text: "https://t.me/chan https://T.ME/chan2 http://sub.t.me/x https://telegram.me/chan3 https://www.telegram.me/y https://ok.example/z"},
			max:  5,
			want: []string{"https://ok.example/z"},
		},
		{
			name: "мусор без http(s)-схемы НЕ извлекается",
			item: feed.Item{Text: "ftp://files.example/x example.com www.example.com просто текст mailto:a@b.example"},
			max:  5,
			want: nil,
		},
		{
			name: "Item.URL без схемы отбрасывается",
			item: feed.Item{URL: "example.com/no-scheme", Text: "https://ok.example/a"},
			max:  5,
			want: []string{"https://ok.example/a"},
		},
		{
			name: ">max — обрезка с сохранением порядка",
			item: feed.Item{Text: "https://a.example https://b.example https://c.example https://d.example"},
			max:  2,
			want: []string{"https://a.example", "https://b.example"},
		},
		{
			name: "max=0 — пусто (защита)",
			item: feed.Item{URL: "https://a.example", Text: "https://b.example"},
			max:  0,
			want: nil,
		},
		{
			name: "max отрицательный — пусто (защита)",
			item: feed.Item{Text: "https://a.example"},
			max:  -1,
			want: nil,
		},
		{
			name: "URL в кавычках и угловых скобках — граница [^\\s<>\"']+",
			item: feed.Item{Text: `<a href="https://a.example/x">link</a> and <https://b.example/y> and 'https://c.example/z'`},
			max:  5,
			want: []string{"https://a.example/x", "https://b.example/y", "https://c.example/z"},
		},
		{
			name: "query сохраняется байт-в-байт",
			item: feed.Item{Text: "https://q.example/search?query=Go+News&lang=ru#results"},
			max:  5,
			want: []string{"https://q.example/search?query=Go+News&lang=ru"},
		},
		{
			name: "path-регистр сохраняется",
			item: feed.Item{Text: "https://p.example/CaseSensitive/Path"},
			max:  5,
			want: []string{"https://p.example/CaseSensitive/Path"},
		},
		{
			name: "корневой trailing slash отбрасывается: http://h/ → http://h",
			item: feed.Item{Text: "http://h.example/ https://h2.example"},
			max:  5,
			want: []string{"http://h.example", "https://h2.example"},
		},
		{
			name: "несколько URL в одном тексте — порядок появления",
			item: feed.Item{Text: "first https://one.example second https://two.example third https://three.example"},
			max:  5,
			want: []string{"https://one.example", "https://two.example", "https://three.example"},
		},
		{
			name: "пустые URL и Text — пусто",
			item: feed.Item{},
			max:  5,
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractURLs(tt.item, tt.max)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("extractURLs() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

// Ре-ревью F7: нормализованный ключ НЕ сохраняет userinfo — креденшелы не
// должны попадать в ключ link_cache.
func TestNormalizeURLDropsUserinfo(t *testing.T) {
	got, ok := normalizeURL("https://user:pass@example.org/p")
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if want := "https://example.org/p"; got != want {
		t.Errorf("normalizeURL() = %q, want %q", got, want)
	}
}

// Техдолг 9B-3 (ре-ревью L6): trailing-пунктуация захватывается в URL —
// граница regex только пробел и <>"'. Тест ДОКУМЕНТИРУЕТ статус-кво от
// случайных регрессий; триггер пересмотра — реальные битые ссылки в дайджесте.
func TestExtractURLsTrailingPunctuationCaptured(t *testing.T) {
	got := extractURLs(feed.Item{Text: "(https://a.example/x)."}, 5)
	if len(got) != 1 || got[0] != "https://a.example/x)." {
		t.Errorf(`extractURLs() = %#v, want ["https://a.example/x)."] (статус-кво техдолга 9B-3)`, got)
	}
}
