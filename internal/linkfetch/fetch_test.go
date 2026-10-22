package linkfetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// htmlHandler serves body with the given Content-Type (header omitted when
// contentType is "").
func htmlHandler(contentType, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		_, _ = io.WriteString(w, body)
	}
}

// 1. HTML со script/style/nav/header/footer/noscript/form/aside → текст БЕЗ
// их содержимого; текст соседних блоков не склеивается.
func TestFetchExtractsTextSkippingNonContentElements(t *testing.T) {
	page := `<html><head><title>Head Title</title>` +
		`<style>body{color:red}</style><script>alert("boom")</script></head>` +
		`<body><nav><a href="/">MenuLink</a></nav><header>HdrText</header>` +
		`<footer>FtrText</footer><noscript>NoScriptText</noscript>` +
		`<aside>SideText</aside><form><input value="x"></form>` +
		`<p>First paragraph</p><p>Second paragraph</p></body></html>`

	ts := httptest.NewServer(htmlHandler("text/html; charset=utf-8", page))
	defer ts.Close()

	f := NewFetcher(ts.Client(), 1<<20)
	p, err := f.Fetch(context.Background(), ts.URL+"/article")
	if err != nil {
		t.Fatalf("Fetch: unexpected error: %v", err)
	}
	for _, want := range []string{"First paragraph", "Second paragraph"} {
		if !strings.Contains(p.Text, want) {
			t.Errorf("Text missing %q, got:\n%s", want, p.Text)
		}
	}
	for _, banned := range []string{
		"boom", "color:red", "MenuLink", "HdrText", "FtrText",
		"NoScriptText", "SideText", "Head Title",
	} {
		if strings.Contains(p.Text, banned) {
			t.Errorf("Text must not contain %q (filtered element), got:\n%s", banned, p.Text)
		}
	}
	if strings.Contains(p.Text, "First paragraphSecond paragraph") {
		t.Errorf("adjacent block text must not be glued, got:\n%s", p.Text)
	}
	if p.URL != ts.URL+"/article" {
		t.Errorf("URL = %q, want %q", p.URL, ts.URL+"/article")
	}
	if p.Title != "Head Title" {
		t.Errorf("Title = %q, want %q", p.Title, "Head Title")
	}
}

// 2. Title из <title>; при пустом/отсутствующем <title> — из первого <h1>.
func TestFetchTitle(t *testing.T) {
	cases := []struct {
		name string
		page string
		want string
	}{
		{
			name: "из title, пробелы trimmed",
			page: `<html><head><title>  My Title  </title></head><body><h1>Ignored</h1></body></html>`,
			want: "My Title",
		},
		{
			name: "пустой title → первый h1",
			page: `<html><head><title>   </title></head><body><h1>Heading One</h1><h1>Second H1</h1></body></html>`,
			want: "Heading One",
		},
		{
			name: "нет title → h1",
			page: `<html><body><h1>H1 Only</h1></body></html>`,
			want: "H1 Only",
		},
		{
			name: "ни title, ни h1 → пусто",
			page: `<html><body><p>just text</p></body></html>`,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(htmlHandler("text/html", tc.page))
			defer ts.Close()

			f := NewFetcher(ts.Client(), 1<<20)
			p, err := f.Fetch(context.Background(), ts.URL)
			if err != nil {
				t.Fatalf("Fetch: unexpected error: %v", err)
			}
			if p.Title != tc.want {
				t.Errorf("Title = %q, want %q", p.Title, tc.want)
			}
		})
	}
}

// 3. Тело > maxBytes → обрезка (выбранное поведение: truncate-and-parse, не
// ошибка): начало прочитано, хвост за лимитом — нет.
func TestFetchTruncatesOversizedBody(t *testing.T) {
	page := "<html><body><p>MARKER_START" + strings.Repeat("x", 5000) + "MARKER_END</p></body></html>"

	ts := httptest.NewServer(htmlHandler("text/html", page))
	defer ts.Close()

	f := NewFetcher(ts.Client(), 200) // 200 байт: MARKER_START влезает, MARKER_END — нет
	p, err := f.Fetch(context.Background(), ts.URL)
	if err != nil {
		t.Fatalf("Fetch on oversized body must truncate, not fail: %v", err)
	}
	if !strings.Contains(p.Text, "MARKER_START") {
		t.Errorf("Text must keep content within maxBytes, got:\n%s", p.Text)
	}
	if strings.Contains(p.Text, "MARKER_END") {
		t.Errorf("Text must not contain bytes beyond maxBytes, got:\n%s", p.Text)
	}
}

// 4 и 8. Content-Type: не-HTML → ошибка; пустой и text/html (с charset, в
// другом регистре) → успех.
func TestFetchContentTypeFilter(t *testing.T) {
	page := `<html><head><title>T</title></head><body><p>body text</p></body></html>`
	cases := []struct {
		name    string
		ct      string // "" — заголовок не отправляется
		wantErr bool
	}{
		{name: "text/html", ct: "text/html", wantErr: false},
		{name: "text/html c charset", ct: "text/html; charset=utf-8", wantErr: false},
		{name: "верхний регистр", ct: "TEXT/HTML", wantErr: false},
		{name: "пустой Content-Type трактуем как html", ct: "", wantErr: false},
		{name: "application/json", ct: "application/json", wantErr: true},
		{name: "application/pdf", ct: "application/pdf", wantErr: true},
		{name: "text/plain", ct: "text/plain", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(htmlHandler(tc.ct, page))
			defer ts.Close()

			f := NewFetcher(ts.Client(), 1<<20)
			_, err := f.Fetch(context.Background(), ts.URL)
			if tc.wantErr && err == nil {
				t.Fatalf("Fetch with content type %q: want error, got nil", tc.ct)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Fetch with content type %q: unexpected error: %v", tc.ct, err)
			}
		})
	}
}

// 5. Редиректы: цепочка из 3 хоппов — успех и Page.URL = финальный; 4 хопа —
// ошибка (CheckRedirect).
func TestFetchRedirectChain(t *testing.T) {
	page := `<html><body><p>final page</p></body></html>`
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		rest, ok := strings.CutPrefix(r.URL.Path, "/hop")
		if !ok {
			http.NotFound(w, r)
			return
		}
		n, err := strconv.Atoi(rest)
		if err != nil || n <= 0 {
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, page)
			return
		}
		http.Redirect(w, r, fmt.Sprintf("/hop%d", n-1), http.StatusFound)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	t.Run("3 хопа — успех, URL финальный", func(t *testing.T) {
		f := NewFetcher(ts.Client(), 1<<20)
		p, err := f.Fetch(context.Background(), ts.URL+"/hop3")
		if err != nil {
			t.Fatalf("Fetch: unexpected error: %v", err)
		}
		if want := ts.URL + "/hop0"; p.URL != want {
			t.Errorf("URL = %q, want final %q", p.URL, want)
		}
		if !strings.Contains(p.Text, "final page") {
			t.Errorf("Text = %q, want final page content", p.Text)
		}
	})

	t.Run("4 хопа — ошибка", func(t *testing.T) {
		f := NewFetcher(ts.Client(), 1<<20)
		if _, err := f.Fetch(context.Background(), ts.URL+"/hop4"); err == nil {
			t.Fatal("Fetch through 4 redirects: want error, got nil")
		}
	})
}

// 6. 404/429/503 (и прочие не-2xx) → ошибка без паники и без ретраев.
func TestFetchErrorStatuses(t *testing.T) {
	for _, code := range []int{404, 429, 503, 403, 500} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			var hits atomic.Int32
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.WriteHeader(code)
			}))
			defer ts.Close()

			f := NewFetcher(ts.Client(), 1<<20)
			_, err := f.Fetch(context.Background(), ts.URL)
			if err == nil {
				t.Fatalf("Fetch with status %d: want error, got nil", code)
			}
			if !strings.Contains(err.Error(), strconv.Itoa(code)) {
				t.Errorf("error %q must mention status %d", err, code)
			}
			if got := hits.Load(); got != 1 {
				t.Errorf("server hit %d times, want exactly 1 (no retries)", got)
			}
		})
	}
}

// 7. Таймаут: медленный сервер + короткий ctx → ошибка context deadline.
func TestFetchContextTimeout(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html><body><p>late</p></body></html>")
	}))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	f := NewFetcher(ts.Client(), 1<<20)
	_, err := f.Fetch(ctx, ts.URL)
	if err == nil {
		t.Fatal("Fetch with expired context: want error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want wrapping context.DeadlineExceeded", err)
	}
}

// 9. User-Agent запроса = ожидаемый вежливый бот-UA.
func TestFetchSendsPoliteUserAgent(t *testing.T) {
	const wantUA = "Mozilla/5.0 (compatible; aiTelegaBot/1.0)"
	gotUA := make(chan string, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA <- r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html><body><p>ok</p></body></html>")
	}))
	defer ts.Close()

	f := NewFetcher(ts.Client(), 1<<20)
	if _, err := f.Fetch(context.Background(), ts.URL); err != nil {
		t.Fatalf("Fetch: unexpected error: %v", err)
	}
	if ua := <-gotUA; ua != wantUA {
		t.Errorf("User-Agent = %q, want %q", ua, wantUA)
	}
}

// 10. Text > fetchTextRunes рун → обрезка до fetchTextRunes на границе рун
// (многобайтовая кириллица: обрезка по байтам дала бы невалидный UTF-8).
func TestFetchTextTruncatedOnRuneBoundary(t *testing.T) {
	page := "<html><body><p>" + strings.Repeat("ф", fetchTextRunes+1000) + "</p></body></html>"

	ts := httptest.NewServer(htmlHandler("text/html", page))
	defer ts.Close()

	f := NewFetcher(ts.Client(), 10<<20)
	p, err := f.Fetch(context.Background(), ts.URL)
	if err != nil {
		t.Fatalf("Fetch: unexpected error: %v", err)
	}
	if got := utf8.RuneCountInString(p.Text); got != fetchTextRunes {
		t.Errorf("len(Text) = %d runes, want exactly %d", got, fetchTextRunes)
	}
	if !utf8.ValidString(p.Text) {
		t.Error("Text is not valid UTF-8 — truncation happened on a byte boundary")
	}
	if want := strings.Repeat("ф", fetchTextRunes); p.Text != want {
		t.Error("Text content after truncation differs from the first fetchTextRunes runes")
	}
}

// Ревью F4: extractTitle обрезает результат до titleRunes=300 рун — длинный
// <title> не раздувает link_cache.title и промпт F.
func TestFetchTitleTruncatedTo300Runes(t *testing.T) {
	long := strings.Repeat("заголовок ", 35) // 10 × 35 = 350 рун
	if n := utf8.RuneCountInString(long); n != 350 {
		t.Fatalf("fixture broken: %d runes, want 350", n)
	}
	srv := httptest.NewServer(htmlHandler("text/html; charset=utf-8",
		"<html><head><title>"+long+"</title></head><body><p>текст</p></body></html>"))
	defer srv.Close()

	page, err := NewFetcher(nil, 0).Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if n := utf8.RuneCountInString(page.Title); n != 300 {
		t.Errorf("len(Title) = %d runes, want 300", n)
	}
	if !utf8.ValidString(page.Title) {
		t.Error("Title is not valid UTF-8 (обрезка не на границе руны)")
	}
}

// Ревью F9c: NewFetcher НЕ мутирует http.Client вызывающего — политика
// CheckRedirect ≤3 хоппов ставится на shallow-copy.
func TestNewFetcherDoesNotMutateCallerClient(t *testing.T) {
	hc := &http.Client{}
	_ = NewFetcher(hc, 0)
	if hc.CheckRedirect != nil {
		t.Error("caller's hc.CheckRedirect was mutated by NewFetcher")
	}
}
