package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// linkMsgs — детерминированный вход вызова E: 2 сообщения, 2+1 ссылок →
// 3 пары (msg,link) всего.
func linkMsgs() []MsgLinks {
	return []MsgLinks{
		{Text: "Пост про Go", URLs: []string{"https://a.example/1", "https://b.example/2"}},
		{Text: "Курс и реклама", URLs: []string{"https://c.example/3"}},
	}
}

const validLinksJSON = `{"links":[{"msg":1,"link":1,"kind":"article"},{"msg":1,"link":2,"kind":"ad"},{"msg":2,"link":1,"kind":"course"}]}`

func TestClientClassifyLinks_ValidJSON(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatBody(validLinksJSON)))
	}))
	defer srv.Close()

	c := NewClient("test-key", srv.URL, "test-model", 1)
	kinds, err := c.ClassifyLinks(context.Background(), linkMsgs())
	if err != nil {
		t.Fatalf("ClassifyLinks: %v", err)
	}
	want := []LinkKind{
		{Msg: 1, Link: 1, Kind: LinkKindArticle},
		{Msg: 1, Link: 2, Kind: LinkKindAd},
		{Msg: 2, Link: 1, Kind: LinkKindCourse},
	}
	if len(kinds) != len(want) {
		t.Fatalf("len(kinds) = %d, want %d (контракт покрытия: ровно по паре на вход)", len(kinds), len(want))
	}
	for i, w := range want {
		if kinds[i] != w {
			t.Errorf("kinds[%d] = %+v, want %+v", i, kinds[i], w)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("server calls = %d, want 1", n)
	}
}

func TestClientClassifyLinks_FencedJSON(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatBody("```json\n" + validLinksJSON + "\n```")))
	}))
	defer srv.Close()

	c := NewClient("test-key", srv.URL, "test-model", 1)
	kinds, err := c.ClassifyLinks(context.Background(), linkMsgs())
	if err != nil {
		t.Fatalf("ClassifyLinks over fenced JSON: %v", err)
	}
	if len(kinds) != 3 || kinds[0].Kind != LinkKindArticle || kinds[2].Kind != LinkKindCourse {
		t.Errorf("kinds = %+v, want the parsed payload", kinds)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("server calls = %d, want 1 (fence-strip, no repeat)", n)
	}
}

// TestParseLinksResponse_Validation: msg/link вне диапазона → отброшены
// (покрытие пар сохранено: отсутствующие → other); неизвестный kind → other;
// регистр нормализуется; дубль пары → первый выигрыш.
func TestParseLinksResponse_Validation(t *testing.T) {
	content := `{"links":[` +
		`{"msg":0,"link":1,"kind":"ad"},` + // msg вне диапазона
		`{"msg":1,"link":3,"kind":"ad"},` + // link вне диапазона
		`{"msg":3,"link":1,"kind":"ad"},` + // msg вне диапазона
		`{"msg":1,"link":1,"kind":"ARTICLE"},` +
		`{"msg":1,"link":1,"kind":"ad"},` + // дубль — первый выигрыш
		`{"msg":1,"link":2,"kind":"review"}` + // неизвестный kind → other
		`]}` // (2,1) отсутствует → other
	kinds, err := parseLinksResponse(content, linkMsgs())
	if err != nil {
		t.Fatalf("parseLinksResponse: %v", err)
	}
	want := []LinkKind{
		{Msg: 1, Link: 1, Kind: LinkKindArticle}, // нормализация регистра; дубль отброшен
		{Msg: 1, Link: 2, Kind: LinkKindOther},   // неизвестный kind → other
		{Msg: 2, Link: 1, Kind: LinkKindOther},   // отсутствующая пара → other
	}
	if len(kinds) != len(want) {
		t.Fatalf("len = %d, want %d (контракт покрытия)", len(kinds), len(want))
	}
	for i, w := range want {
		if kinds[i] != w {
			t.Errorf("kinds[%d] = %+v, want %+v", i, kinds[i], w)
		}
	}
}

func TestClientClassifyLinks_GarbageErrorsAfterTwoRequests(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatBody("это не JSON, а обычный текст")))
	}))
	defer srv.Close()

	c := NewClient("test-key", srv.URL, "test-model", 1)
	kinds, err := c.ClassifyLinks(context.Background(), linkMsgs())
	if err == nil {
		t.Fatalf("ClassifyLinks succeeded with kinds=%+v, want error (двойной мусор → error, БЕЗ fallback)", kinds)
	}
	if kinds != nil {
		t.Errorf("kinds = %+v, want nil alongside the error", kinds)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("server calls = %d, want exactly 2 (initial + one repeat)", n)
	}
}

func TestClientClassifyLinks_EmptyLinksArrayIsGarbage(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatBody(`{"links":[]}`)))
	}))
	defer srv.Close()

	c := NewClient("test-key", srv.URL, "test-model", 1)
	if _, err := c.ClassifyLinks(context.Background(), linkMsgs()); err == nil {
		t.Fatal("ClassifyLinks on empty links array: want error (решение 9A №8)")
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("server calls = %d, want exactly 2", n)
	}
}

func TestClientClassifyLinks_TransportError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest) // fatal — не ретраится
		_, _ = w.Write([]byte(`{"error":{"message":"bad request"}}`))
	}))
	defer srv.Close()

	c := NewClient("test-key", srv.URL, "test-model", 1)
	if _, err := c.ClassifyLinks(context.Background(), linkMsgs()); err == nil {
		t.Fatal("ClassifyLinks succeeded, want transport error")
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("server calls = %d, want 1 (400 is fatal — not retried)", n)
	}
}

// TestClientClassifyLinks_PromptTruncation: текст сообщения >200 рун обрезан
// в промпте (assert на содержимом запроса); URLs присутствуют детерминированно.
func TestClientClassifyLinks_PromptTruncation(t *testing.T) {
	longText := strings.Repeat("а", 250)
	msgs := []MsgLinks{{Text: longText, URLs: []string{"https://a.example/1"}}}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		content := req.Messages[1].Content
		if !strings.Contains(content, strings.Repeat("а", linkTextRunes)) ||
			strings.Contains(content, strings.Repeat("а", linkTextRunes+1)) {
			t.Errorf("текст сообщения не обрезан до %d рун:\n%s", linkTextRunes, content)
		}
		if !strings.Contains(content, "[1] https://a.example/1") {
			t.Errorf("URL не пронумерован детерминированно:\n%s", content)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatBody(`{"links":[{"msg":1,"link":1,"kind":"article"}]}`)))
	}))
	defer srv.Close()

	c := NewClient("test-key", srv.URL, "test-model", 1)
	kinds, err := c.ClassifyLinks(context.Background(), msgs)
	if err != nil {
		t.Fatalf("ClassifyLinks: %v", err)
	}
	if len(kinds) != 1 || kinds[0].Kind != LinkKindArticle {
		t.Errorf("kinds = %+v, want one article", kinds)
	}
}

func TestClientClassifyLinks_EmptyInputNoRequest(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatBody(validLinksJSON)))
	}))
	defer srv.Close()

	c := NewClient("test-key", srv.URL, "test-model", 1)
	// Ни одного сообщения и сообщение без URL — пар нет, HTTP-запроса нет.
	for name, in := range map[string][]MsgLinks{
		"nil":        nil,
		"no-urls":    {{Text: "пост без ссылок"}},
		"empty-msgs": {},
	} {
		kinds, err := c.ClassifyLinks(context.Background(), in)
		if err != nil {
			t.Fatalf("ClassifyLinks(%s): %v", name, err)
		}
		if len(kinds) != 0 {
			t.Errorf("ClassifyLinks(%s) = %+v, want empty", name, kinds)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("server calls = %d, want 0 (пустой вход — без HTTP-запроса)", n)
	}
}

func TestClassifyLinks_NoKeyInLogs(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(orig)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatBody("мусор")))
	}))
	defer srv.Close()

	const secret = "SECRET_KEY_123"
	c := NewClient(secret, srv.URL, "m", 2)
	if _, err := c.ClassifyLinks(context.Background(), linkMsgs()); err == nil {
		t.Fatal("ClassifyLinks on garbage: want error")
	}
	if strings.Contains(buf.String(), secret) {
		t.Fatalf("api key leaked into logs: %s", buf.String())
	}
}

// --- Вызов F ---

func linkPages(n int) []LinkPage {
	pages := make([]LinkPage, n)
	for i := range pages {
		pages[i] = LinkPage{
			URL:   "https://a.example/" + string(rune('1'+i)),
			Title: "Статья " + string(rune('1'+i)),
			Text:  "Текст статьи " + string(rune('1'+i)),
		}
	}
	return pages
}

func TestClientSummarizeLinks_ValidJSONMissingIndexEmpty(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		// индекс 2 ОТСУТСТВУЕТ → Summary="" (не ошибка); индекс 3 — пустое
		// summary от LLM → остаётся "" (не fallback).
		_, _ = w.Write([]byte(chatBody(`{"items":[{"index":1,"summary":"s1"},{"index":3,"summary":""}]}`)))
	}))
	defer srv.Close()

	c := NewClient("test-key", srv.URL, "test-model", 1)
	sums, err := c.SummarizeLinks(context.Background(), linkPages(3))
	if err != nil {
		t.Fatalf("SummarizeLinks: %v", err)
	}
	want := []LinkSummary{{Index: 1, Summary: "s1"}, {Index: 2, Summary: ""}, {Index: 3, Summary: ""}}
	if len(sums) != len(want) {
		t.Fatalf("len(sums) = %d, want %d (контракт покрытия: ровно по странице)", len(sums), len(want))
	}
	for i, w := range want {
		if sums[i] != w {
			t.Errorf("sums[%d] = %+v, want %+v", i, sums[i], w)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("server calls = %d, want 1", n)
	}
}

func TestClientSummarizeLinks_FencedJSON(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatBody("```\n" + `{"items":[{"index":1,"summary":"s1"}]}` + "\n```")))
	}))
	defer srv.Close()

	c := NewClient("test-key", srv.URL, "test-model", 1)
	sums, err := c.SummarizeLinks(context.Background(), linkPages(1))
	if err != nil {
		t.Fatalf("SummarizeLinks over fenced JSON: %v", err)
	}
	if len(sums) != 1 || sums[0].Summary != "s1" {
		t.Errorf("sums = %+v, want [1 s1]", sums)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("server calls = %d, want 1", n)
	}
}

// TestParseLinkSummaries_Validation: index вне 1..n → отброшен, дубль →
// первый выигрыш (parseTopicSummary-паттерн).
func TestParseLinkSummaries_Validation(t *testing.T) {
	content := `{"items":[` +
		`{"index":0,"summary":"zero"},` +
		`{"index":4,"summary":"out"},` +
		`{"index":2,"summary":"ok2"},` +
		`{"index":2,"summary":"dup"},` +
		`{"index":1,"summary":"ok1"}]}`
	sums, err := parseLinkSummaries(content, 3)
	if err != nil {
		t.Fatalf("parseLinkSummaries: %v", err)
	}
	want := []LinkSummary{{Index: 1, Summary: "ok1"}, {Index: 2, Summary: "ok2"}, {Index: 3, Summary: ""}}
	if len(sums) != len(want) {
		t.Fatalf("len = %d, want %d", len(sums), len(want))
	}
	for i, w := range want {
		if sums[i] != w {
			t.Errorf("sums[%d] = %+v, want %+v", i, sums[i], w)
		}
	}
}

// TestClientSummarizeLinks_PromptTruncation: Text >1500 рун обрезан в промпте
// (assert на содержимом запроса), Title присутствует.
func TestClientSummarizeLinks_PromptTruncation(t *testing.T) {
	pages := []LinkPage{{URL: "https://a.example/1", Title: "Длинная статья", Text: strings.Repeat("т", 1600)}}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		content := req.Messages[1].Content
		if !strings.Contains(content, strings.Repeat("т", linkSummaryTextRunes)) ||
			strings.Contains(content, strings.Repeat("т", linkSummaryTextRunes+1)) {
			t.Errorf("fetched-текст не обрезан до %d рун", linkSummaryTextRunes)
		}
		if !strings.Contains(content, "1. Длинная статья") {
			t.Errorf("Title отсутствует в промпте:\n%s", content[:min(len(content), 120)])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatBody(`{"items":[{"index":1,"summary":"s1"}]}`)))
	}))
	defer srv.Close()

	c := NewClient("test-key", srv.URL, "test-model", 1)
	sums, err := c.SummarizeLinks(context.Background(), pages)
	if err != nil {
		t.Fatalf("SummarizeLinks: %v", err)
	}
	if len(sums) != 1 || sums[0].Summary != "s1" {
		t.Errorf("sums = %+v, want [1 s1]", sums)
	}
}

func TestClientSummarizeLinks_GarbageErrorsAfterTwoRequests(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatBody("мусор без JSON")))
	}))
	defer srv.Close()

	c := NewClient("test-key", srv.URL, "test-model", 1)
	sums, err := c.SummarizeLinks(context.Background(), linkPages(2))
	if err == nil {
		t.Fatalf("SummarizeLinks succeeded with sums=%+v, want error (двойной мусор → error)", sums)
	}
	if sums != nil {
		t.Errorf("sums = %+v, want nil alongside the error", sums)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("server calls = %d, want exactly 2 (initial + one repeat)", n)
	}
}

func TestSummarizeLinks_NoKeyInLogs(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(orig)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatBody("мусор")))
	}))
	defer srv.Close()

	const secret = "SECRET_KEY_123"
	c := NewClient(secret, srv.URL, "m", 2)
	if _, err := c.SummarizeLinks(context.Background(), linkPages(1)); err == nil {
		t.Fatal("SummarizeLinks on garbage: want error")
	}
	if strings.Contains(buf.String(), secret) {
		t.Fatalf("api key leaked into logs: %s", buf.String())
	}
}

// --- Offline ---

func TestOfflineLinks(t *testing.T) {
	msgs := linkMsgs()
	pages := linkPages(3)
	o := NewOffline()

	a1, err := o.ClassifyLinks(context.Background(), msgs)
	if err != nil {
		t.Fatalf("ClassifyLinks: %v", err)
	}
	a2, err := o.ClassifyLinks(context.Background(), msgs)
	if err != nil {
		t.Fatalf("ClassifyLinks (2nd call): %v", err)
	}
	ja, _ := json.Marshal(a1)
	jb, _ := json.Marshal(a2)
	if !bytes.Equal(ja, jb) {
		t.Errorf("два offline-вызова ClassifyLinks различаются:\n%s\n%s", ja, jb)
	}
	wantKinds := []LinkKind{
		{Msg: 1, Link: 1, Kind: LinkKindOther},
		{Msg: 1, Link: 2, Kind: LinkKindOther},
		{Msg: 2, Link: 1, Kind: LinkKindOther},
	}
	if len(a1) != len(wantKinds) {
		t.Fatalf("len(kinds) = %d, want %d (контракт покрытия)", len(a1), len(wantKinds))
	}
	for i, w := range wantKinds {
		if a1[i] != w {
			t.Errorf("kinds[%d] = %+v, want %+v (offline — все other)", i, a1[i], w)
		}
	}

	b1, err := o.SummarizeLinks(context.Background(), pages)
	if err != nil {
		t.Fatalf("SummarizeLinks: %v", err)
	}
	b2, err := o.SummarizeLinks(context.Background(), pages)
	if err != nil {
		t.Fatalf("SummarizeLinks (2nd call): %v", err)
	}
	jb1, _ := json.Marshal(b1)
	jb2, _ := json.Marshal(b2)
	if !bytes.Equal(jb1, jb2) {
		t.Errorf("два offline-вызова SummarizeLinks различаются:\n%s\n%s", jb1, jb2)
	}
	if len(b1) != 3 {
		t.Fatalf("len(sums) = %d, want 3 (контракт покрытия)", len(b1))
	}
	for i, s := range b1 {
		if s.Index != i+1 || s.Summary != "" {
			t.Errorf("sums[%d] = %+v, want Index=%d Summary=\"\"", i, s, i+1)
		}
	}

	// Пустой вход — пустой результат без ошибки.
	e1, err := o.ClassifyLinks(context.Background(), nil)
	if err != nil || len(e1) != 0 {
		t.Errorf("ClassifyLinks(nil) = %+v, %v", e1, err)
	}
	e2, err := o.SummarizeLinks(context.Background(), nil)
	if err != nil || len(e2) != 0 {
		t.Errorf("SummarizeLinks(nil) = %+v, %v", e2, err)
	}
}
