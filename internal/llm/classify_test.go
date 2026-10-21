package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// classifyItems builds n items with a deterministic two-sentence Text:
// extractLead cuts at the first ". " → lead = "Первое предложение <i>.".
func classifyItems(n int) []ClassifyItem {
	items := make([]ClassifyItem, n)
	for i := range items {
		items[i] = ClassifyItem{
			Title: "T" + strconv.Itoa(i+1),
			Text:  "Первое предложение " + strconv.Itoa(i+1) + ". Второе предложение",
		}
	}
	return items
}

const validClassifyJSON = `{"items":[{"index":1,"class":"news","summary":"s1"},{"index":2,"class":"course","summary":"Курс: Go — основы"}]}`

func TestClientClassifySummarize_ValidJSON(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatBody(validClassifyJSON)))
	}))
	defer srv.Close()

	c := NewClient("test-key", srv.URL, "test-model", 1)
	res, err := c.ClassifySummarize(context.Background(), classifyItems(2))
	if err != nil {
		t.Fatalf("ClassifySummarize: %v", err)
	}
	want := []ItemClassSummary{
		{Index: 1, Class: ClassNews, Summary: "s1"},
		{Index: 2, Class: ClassCourse, Summary: "Курс: Go — основы"},
	}
	if len(res.Items) != 2 || res.Items[0] != want[0] || res.Items[1] != want[1] {
		t.Errorf("Items = %+v, want %+v", res.Items, want)
	}
	if len(res.Degraded) != 2 || res.Degraded[0] || res.Degraded[1] {
		t.Errorf("Degraded = %v, want [false false]", res.Degraded)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("server calls = %d, want 1", n)
	}
}

func TestClientClassifySummarize_FencedJSON(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatBody("```json\n" + validClassifyJSON + "\n```")))
	}))
	defer srv.Close()

	c := NewClient("test-key", srv.URL, "test-model", 1)
	res, err := c.ClassifySummarize(context.Background(), classifyItems(2))
	if err != nil {
		t.Fatalf("ClassifySummarize over fenced JSON: %v", err)
	}
	if len(res.Items) != 2 || res.Items[0].Class != ClassNews || res.Items[1].Class != ClassCourse {
		t.Errorf("res = %+v, want the parsed payload", res)
	}
	if res.Degraded[0] || res.Degraded[1] {
		t.Errorf("Degraded = %v, want all false", res.Degraded)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("server calls = %d, want 1 (fence-strip, no repeat)", n)
	}
}

func TestClientClassifySummarize_UnknownClassBecomesOther(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatBody(`{"items":[{"index":1,"class":"review","summary":"s1"},{"index":2,"class":"NEWS","summary":"s2"}]}`)))
	}))
	defer srv.Close()

	c := NewClient("test-key", srv.URL, "test-model", 1)
	res, err := c.ClassifySummarize(context.Background(), classifyItems(2))
	if err != nil {
		t.Fatalf("ClassifySummarize: %v", err)
	}
	if res.Items[0].Class != ClassOther {
		t.Errorf("unknown class → %q, want %q", res.Items[0].Class, ClassOther)
	}
	if res.Items[1].Class != ClassNews {
		t.Errorf("case-normalized class → %q, want %q", res.Items[1].Class, ClassNews)
	}
	if res.Degraded[0] || res.Degraded[1] {
		t.Errorf("Degraded = %v, want all false (normalized answer is not degradation)", res.Degraded)
	}
}

func TestParseClassifyResponse_IndexValidation(t *testing.T) {
	items := classifyItems(3)
	// index 0 and 4 are out of range → dropped; index 2 appears twice → first
	// wins; index 3 is missing → degraded fill (other + extractLead).
	content := `{"items":[` +
		`{"index":0,"class":"ad","summary":"x"},` +
		`{"index":4,"class":"ad","summary":"x"},` +
		`{"index":2,"class":"news","summary":"ok2"},` +
		`{"index":2,"class":"ad","summary":"dup"},` +
		`{"index":1,"class":"news","summary":"ok1"}]}`
	res, err := parseClassifyResponse(content, items)
	if err != nil {
		t.Fatalf("parseClassifyResponse: %v", err)
	}
	want := []ItemClassSummary{
		{Index: 1, Class: ClassNews, Summary: "ok1"},
		{Index: 2, Class: ClassNews, Summary: "ok2"}, // первый выигрыш, не "dup"
		{Index: 3, Class: ClassOther, Summary: "Первое предложение 3."},
	}
	for i, w := range want {
		if res.Items[i] != w {
			t.Errorf("Items[%d] = %+v, want %+v", i, res.Items[i], w)
		}
	}
	if res.Degraded[0] || res.Degraded[1] {
		t.Errorf("Degraded[0..1] = %v, want false", res.Degraded[:2])
	}
	if !res.Degraded[2] {
		t.Error("Degraded[2] = false, want true (missing index → deterministic degradation)")
	}
}

func TestClientClassifySummarize_GarbageFallsBackAfterTwoRequests(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatBody("это не JSON, а обычный текст")))
	}))
	defer srv.Close()

	items := []ClassifyItem{
		{Title: "T1", Text: "Первое предложение. Второе"},
		{Title: "T2", Text: ""},
	}
	c := NewClient("test-key", srv.URL, "test-model", 1)
	res, err := c.ClassifySummarize(context.Background(), items)
	if err != nil {
		t.Fatalf("ClassifySummarize on garbage: %v (fallback must not be an error)", err)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("server calls = %d, want exactly 2 (initial + one repeat)", n)
	}
	want := []ItemClassSummary{
		{Index: 1, Class: ClassOther, Summary: "Первое предложение."},
		{Index: 2, Class: ClassOther, Summary: "T2"}, // пустой Text → Title
	}
	if len(res.Items) != 2 || res.Items[0] != want[0] || res.Items[1] != want[1] {
		t.Errorf("Items = %+v, want fallback %+v", res.Items, want)
	}
	if !res.Degraded[0] || !res.Degraded[1] {
		t.Errorf("Degraded = %v, want all true (fallback must be distinguishable)", res.Degraded)
	}
}

func TestClientClassifySummarize_TransportError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest) // fatal — not retried
		_, _ = w.Write([]byte(`{"error":{"message":"bad request"}}`))
	}))
	defer srv.Close()

	c := NewClient("test-key", srv.URL, "test-model", 1)
	if _, err := c.ClassifySummarize(context.Background(), classifyItems(2)); err == nil {
		t.Fatal("ClassifySummarize succeeded, want transport error")
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("server calls = %d, want 1 (400 is fatal — not retried)", n)
	}
}

func TestClassifySummarize_NoKeyInLogs(t *testing.T) {
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
	if _, err := c.ClassifySummarize(context.Background(), classifyItems(1)); err != nil {
		t.Fatalf("ClassifySummarize: %v", err)
	}
	if strings.Contains(buf.String(), secret) {
		t.Fatalf("api key leaked into logs: %s", buf.String())
	}
}

func TestClientClassifySummarize_Chunking(t *testing.T) {
	const total = 125 // > classifyChunkSize (60) → 3 chunks: 60 + 60 + 5
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		// Count numbered items in the user message (Text is empty in this
		// test → exactly one "\n<i>. " marker per item).
		content := req.Messages[1].Content
		n := 0
		for i := 1; strings.Contains(content, "\n"+strconv.Itoa(i)+". "); i++ {
			n = i
		}
		var b strings.Builder
		b.WriteString(`{"items":[`)
		for i := 1; i <= n; i++ {
			if i > 1 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `{"index":%d,"class":"news","summary":"s%d"}`, i, i)
		}
		b.WriteString(`]}`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatBody(b.String())))
	}))
	defer srv.Close()

	items := make([]ClassifyItem, total)
	for i := range items {
		items[i] = ClassifyItem{Title: "T" + strconv.Itoa(i+1)}
	}
	c := NewClient("test-key", srv.URL, "test-model", 1)
	res, err := c.ClassifySummarize(context.Background(), items)
	if err != nil {
		t.Fatalf("ClassifySummarize: %v", err)
	}
	if n := calls.Load(); n != 3 {
		t.Errorf("server calls = %d, want 3 (chunks of %d)", n, classifyChunkSize)
	}
	if len(res.Items) != total || len(res.Degraded) != total {
		t.Fatalf("len(Items)=%d len(Degraded)=%d, want %d/%d", len(res.Items), len(res.Degraded), total, total)
	}
	for i, it := range res.Items {
		if it.Index != i+1 {
			t.Fatalf("Items[%d].Index = %d, want %d (global, input order)", i, it.Index, i+1)
		}
		if it.Class != ClassNews || res.Degraded[i] {
			t.Fatalf("Items[%d] = %+v (Degraded=%v), want parsed news", i, it, res.Degraded[i])
		}
	}
	// Локальные индексы чанков отображены в глобальные: последний элемент
	// чанка 1 → "s60", первый элемент чанка 2 → "s1", последний чанк из 5 → "s5".
	if res.Items[59].Summary != "s60" || res.Items[60].Summary != "s1" || res.Items[124].Summary != "s5" {
		t.Errorf("chunk boundary summaries = %q/%q/%q, want s60/s1/s5",
			res.Items[59].Summary, res.Items[60].Summary, res.Items[124].Summary)
	}
}

func TestOfflineClassifySummarize(t *testing.T) {
	items := []ClassifyItem{
		{Title: "T1", Text: "Первое предложение. Второе"},
		{Title: "T2", Text: ""},
	}
	o := NewOffline()
	a, err := o.ClassifySummarize(context.Background(), items)
	if err != nil {
		t.Fatalf("ClassifySummarize: %v", err)
	}
	b, err := o.ClassifySummarize(context.Background(), items)
	if err != nil {
		t.Fatalf("ClassifySummarize (2nd call): %v", err)
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if !bytes.Equal(ja, jb) {
		t.Errorf("two offline calls differ:\n%s\n%s", ja, jb)
	}
	want := []ItemClassSummary{
		{Index: 1, Class: ClassOther, Summary: "Первое предложение."},
		{Index: 2, Class: ClassOther, Summary: "T2"}, // пустой Text → Title
	}
	if len(a.Items) != 2 || a.Items[0] != want[0] || a.Items[1] != want[1] {
		t.Errorf("Items = %+v, want %+v", a.Items, want)
	}
	if !a.Degraded[0] || !a.Degraded[1] {
		t.Errorf("Degraded = %v, want all true (offline — деградация by design)", a.Degraded)
	}

	// Пустой вход — пустой результат без ошибки.
	empty, err := o.ClassifySummarize(context.Background(), nil)
	if err != nil || len(empty.Items) != 0 {
		t.Errorf("empty input: res=%+v err=%v", empty, err)
	}
}

// --- M1: транспорт-сбой позднего чанка → частичный результат с глобальными индексами ---

func TestClientClassifySummarize_PartialResultOnTransportError(t *testing.T) {
	const total = 125 // 3 чанка: 60 + 60 + 5; чанк 1 успешен, чанк 2 — транспорт-сбой
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if n := calls.Add(1); n > 1 {
			w.WriteHeader(http.StatusBadRequest) // fatal — doWithRetry не повторяет
			_, _ = w.Write([]byte(`{"error":{"message":"bad request"}}`))
			return
		}
		var b strings.Builder
		b.WriteString(`{"items":[`)
		for i := 1; i <= classifyChunkSize; i++ {
			if i > 1 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `{"index":%d,"class":"news","summary":"s%d"}`, i, i)
		}
		b.WriteString(`]}`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatBody(b.String())))
	}))
	defer srv.Close()

	c := NewClient("test-key", srv.URL, "test-model", 1)
	res, err := c.ClassifySummarize(context.Background(), classifyItems(total))
	if err == nil {
		t.Fatal("ClassifySummarize succeeded, want transport error on chunk 2")
	}
	if len(res.Items) != classifyChunkSize || len(res.Degraded) != classifyChunkSize {
		t.Fatalf("partial len(Items)=%d len(Degraded)=%d, want %d/%d",
			len(res.Items), len(res.Degraded), classifyChunkSize, classifyChunkSize)
	}
	for i, it := range res.Items {
		if it.Index != i+1 {
			t.Fatalf("Items[%d].Index = %d, want %d (глобальные индексы 1..60)", i, it.Index, i+1)
		}
		if it.Class != ClassNews || res.Degraded[i] {
			t.Fatalf("Items[%d] = %+v (Degraded=%v), want parsed news", i, it, res.Degraded[i])
		}
	}
}

// --- L2: валидный JSON с пустым items — мусор: ровно один повтор, затем fallback ---

func TestClientClassifySummarize_EmptyItemsArrayFallsBack(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatBody(`{"items":[]}`)))
	}))
	defer srv.Close()

	c := NewClient("test-key", srv.URL, "test-model", 1)
	res, err := c.ClassifySummarize(context.Background(), classifyItems(2))
	if err != nil {
		t.Fatalf("ClassifySummarize: %v (пустой items → fallback, не ошибка)", err)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("server calls = %d, want exactly 2 (initial + one repeat)", n)
	}
	want := []ItemClassSummary{
		{Index: 1, Class: ClassOther, Summary: "Первое предложение 1."},
		{Index: 2, Class: ClassOther, Summary: "Первое предложение 2."},
	}
	if len(res.Items) != 2 || res.Items[0] != want[0] || res.Items[1] != want[1] {
		t.Errorf("Items = %+v, want fallback %+v", res.Items, want)
	}
	if !res.Degraded[0] || !res.Degraded[1] {
		t.Errorf("Degraded = %v, want all true", res.Degraded)
	}
}

// --- L5: пустое summary при валидном индексе → элемент остаётся Degraded ---

func TestParseClassifyResponse_EmptySummaryStaysDegraded(t *testing.T) {
	items := classifyItems(2)

	// Пустое summary на валидный индекс — НЕ заполняем: fallback-предзаполнение
	// (other + extractLead) сохраняется, Degraded=true.
	res, err := parseClassifyResponse(
		`{"items":[{"index":1,"class":"news","summary":""},{"index":2,"class":"news","summary":"ok2"}]}`, items)
	if err != nil {
		t.Fatalf("parseClassifyResponse: %v", err)
	}
	if !res.Degraded[0] {
		t.Error("Degraded[0] = false, want true (пустое summary не заполняется)")
	}
	if want := (ItemClassSummary{Index: 1, Class: ClassOther, Summary: "Первое предложение 1."}); res.Items[0] != want {
		t.Errorf("Items[0] = %+v, want degraded fallback %+v", res.Items[0], want)
	}
	if res.Degraded[1] || res.Items[1].Summary != "ok2" {
		t.Errorf("Items[1] = %+v (Degraded=%v), want filled ok2", res.Items[1], res.Degraded[1])
	}

	// Дубль индекса: первый с пустым summary, второй с текстом → заполнен вторым.
	res2, err := parseClassifyResponse(
		`{"items":[{"index":1,"class":"news","summary":"  "},{"index":1,"class":"article","summary":"filled"}]}`, items)
	if err != nil {
		t.Fatalf("parseClassifyResponse (dup): %v", err)
	}
	if res2.Degraded[0] {
		t.Error("Degraded[0] = true, want false (дубль с непустым summary заполняет)")
	}
	if want := (ItemClassSummary{Index: 1, Class: ClassArticle, Summary: "filled"}); res2.Items[0] != want {
		t.Errorf("Items[0] = %+v, want %+v", res2.Items[0], want)
	}
}

// --- I2: buildClassifyPrompt — нумерованный формат, title/text ≤200 рун ---

func TestBuildClassifyPrompt_FormatAndTruncation(t *testing.T) {
	longTitle := strings.Repeat("т", 250)
	longText := strings.Repeat("а", 250)
	items := []ClassifyItem{
		{Title: longTitle, Text: longText},
		{Title: "B", Text: ""}, // пустой Text — без лишней строки
	}
	msgs := buildClassifyPrompt(items)
	if len(msgs) != 2 || msgs[0].Role != "system" || msgs[0].Content != classifySystemPrompt {
		t.Fatalf("system message unexpected: %+v", msgs[0])
	}
	content := msgs[1].Content
	if msgs[1].Role != "user" || !strings.HasPrefix(content, "Материалы:\n1. ") {
		t.Errorf("numbered format broken:\n%s", content)
	}
	if !strings.Contains(content, strings.Repeat("т", 200)) || strings.Contains(content, strings.Repeat("т", 201)) {
		t.Error("title не обрезан до 200 рун")
	}
	if !strings.Contains(content, strings.Repeat("а", 200)) || strings.Contains(content, strings.Repeat("а", 201)) {
		t.Error("text не обрезан до 200 рун")
	}
	if !strings.HasSuffix(content, "\n2. B") {
		t.Errorf("пустой Text дал лишнюю строку; content заканчивается %q", content[max(0, len(content)-12):])
	}
}
