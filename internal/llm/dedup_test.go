// Тесты вызова B (Этап 9C, план §4.4, тест-кейс 1): httptest-сервер для
// Client.FindDuplicates (валидные кластеры, fence, мусор → РОВНО 2 запроса →
// error, ключ не в логах) + табличные тесты parseDedupResponse (индексы вне
// 1..n, пересечения, primary ∉ indices, одиночные) + Offline-дедуп
// (нормализация заголовка, детерминизм байт-в-байт).

package llm

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// dedupItemsN — детерминированный вход вызова B: n элементов с разными
// заголовками/источниками/датами.
func dedupItemsN(n int) []DedupItem {
	base := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	items := make([]DedupItem, n)
	for i := range items {
		items[i] = DedupItem{
			Title:      "Новость " + itoa(i+1),
			Text:       "Лид новости " + itoa(i+1),
			SourceName: "Источник " + itoa(i+1),
			Published:  base.Add(time.Duration(i) * time.Hour),
			SourceID:   int64(i + 1),
		}
	}
	return items
}

const validDedupJSON = `{"clusters":[{"indices":[1,3],"primary":3},{"indices":[2,4],"primary":2}]}`

func TestClientFindDuplicates_ValidJSON(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatBody(validDedupJSON)))
	}))
	defer srv.Close()

	c := NewClient("test-key", srv.URL, "test-model", 1)
	clusters, err := c.FindDuplicates(context.Background(), dedupItemsN(4))
	if err != nil {
		t.Fatalf("FindDuplicates: %v", err)
	}
	want := []DupCluster{
		{Indices: []int{1, 3}, Primary: 3},
		{Indices: []int{2, 4}, Primary: 2},
	}
	if len(clusters) != len(want) {
		t.Fatalf("clusters = %+v, want %+v", clusters, want)
	}
	for i, w := range want {
		if clusters[i].Primary != w.Primary || len(clusters[i].Indices) != len(w.Indices) {
			t.Fatalf("clusters[%d] = %+v, want %+v", i, clusters[i], w)
		}
		for j, v := range w.Indices {
			if clusters[i].Indices[j] != v {
				t.Fatalf("clusters[%d] = %+v, want %+v", i, clusters[i], w)
			}
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("server calls = %d, want 1 (один запрос на тему, без чанкования)", n)
	}
}

func TestClientFindDuplicates_FencedJSON(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatBody("```json\n" + validDedupJSON + "\n```")))
	}))
	defer srv.Close()

	c := NewClient("test-key", srv.URL, "test-model", 1)
	clusters, err := c.FindDuplicates(context.Background(), dedupItemsN(4))
	if err != nil {
		t.Fatalf("FindDuplicates over fenced JSON: %v", err)
	}
	if len(clusters) != 2 || clusters[0].Primary != 3 {
		t.Errorf("clusters = %+v, want the parsed payload", clusters)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("server calls = %d, want 1 (fence-strip, no repeat)", n)
	}
}

// TestParseDedupResponse_Validation: индексы вне 1..n → отброшены; дубль
// внутри кластера → первый выигрыш; primary ∉ indices → первый индекс;
// пересечение между кластерами → ВТОРОЙ отброшен целиком; <2 валидных →
// отброшен; порядок ответа сохранён.
func TestParseDedupResponse_Validation(t *testing.T) {
	content := `{"clusters":[` +
		`{"indices":[4,5],"primary":5},` + // валиден, первый в ответе
		`{"indices":[1,9,2,2],"primary":7},` + // 9 вне 1..5; дубль 2; primary 7 ∉ → 1
		`{"indices":[2,3],"primary":2},` + // пересечение с предыдущим (2) → отброшен ЦЕЛИКОМ
		`{"indices":[4],"primary":4},` + // 4 занят первым + <2 → отброшен
		`{"indices":[5,5],"primary":5}` + // после дедупа [5] — <2 → отброшен
		`]}`
	clusters, err := parseDedupResponse(content, 5)
	if err != nil {
		t.Fatalf("parseDedupResponse: %v", err)
	}
	want := []DupCluster{
		{Indices: []int{4, 5}, Primary: 5},
		{Indices: []int{1, 2}, Primary: 1},
	}
	if len(clusters) != len(want) {
		t.Fatalf("clusters = %+v, want %+v", clusters, want)
	}
	for i, w := range want {
		if clusters[i].Primary != w.Primary || len(clusters[i].Indices) != len(w.Indices) {
			t.Fatalf("clusters[%d] = %+v, want %+v", i, clusters[i], w)
		}
		for j, v := range w.Indices {
			if clusters[i].Indices[j] != v {
				t.Fatalf("clusters[%d] = %+v, want %+v", i, clusters[i], w)
			}
		}
	}
}

// TestParseDedupResponse_EmptyClustersValid: {"clusters":[]} — валидный ответ
// («дублей нет»), НЕ мусор: без ошибки и без повода для повтора.
func TestParseDedupResponse_EmptyClustersValid(t *testing.T) {
	clusters, err := parseDedupResponse(`{"clusters":[]}`, 3)
	if err != nil {
		t.Fatalf("parseDedupResponse: %v", err)
	}
	if len(clusters) != 0 {
		t.Errorf("clusters = %+v, want empty", clusters)
	}
}

func TestClientFindDuplicates_GarbageErrorsAfterTwoRequests(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatBody("это не JSON, а обычный текст")))
	}))
	defer srv.Close()

	c := NewClient("test-key", srv.URL, "test-model", 1)
	clusters, err := c.FindDuplicates(context.Background(), dedupItemsN(3))
	if err == nil {
		t.Fatalf("FindDuplicates succeeded with %+v, want error (двойной мусор → error, БЕЗ fallback)", clusters)
	}
	if clusters != nil {
		t.Errorf("clusters = %+v, want nil alongside the error", clusters)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("server calls = %d, want exactly 2 (initial + one repeat)", n)
	}
}

func TestClientFindDuplicates_EmptyInputNoRequest(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatBody(validDedupJSON)))
	}))
	defer srv.Close()

	c := NewClient("test-key", srv.URL, "test-model", 1)
	clusters, err := c.FindDuplicates(context.Background(), nil)
	if err != nil {
		t.Fatalf("FindDuplicates(nil): %v", err)
	}
	if clusters != nil {
		t.Errorf("clusters = %+v, want nil", clusters)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("server calls = %d, want 0 (пустой вход — без HTTP)", n)
	}
}

func TestFindDuplicates_NoKeyInLogs(t *testing.T) {
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
	if _, err := c.FindDuplicates(context.Background(), dedupItemsN(2)); err == nil {
		t.Fatal("FindDuplicates succeeded, want double-garbage error")
	}
	if strings.Contains(buf.String(), secret) {
		t.Fatalf("api key leaked into logs: %s", buf.String())
	}
}

// --- Offline (тест-кейс 2 плана 9C) ---

func TestOfflineFindDuplicates_SameTitles(t *testing.T) {
	o := NewOffline()
	items := []DedupItem{
		{Title: "Go 1.24 Released!", SourceID: 3},
		{Title: "Другая новость", SourceID: 1},
		{Title: "go 1.24   released", SourceID: 2}, // регистр + пробелы + пунктуация differ
		{Title: "", SourceID: 4},                   // пустой заголовок не кластеризуется
		{Title: "...", SourceID: 5},                // нормализуется в "" — тоже
	}
	clusters, err := o.FindDuplicates(context.Background(), items)
	if err != nil {
		t.Fatalf("FindDuplicates: %v", err)
	}
	if len(clusters) != 1 {
		t.Fatalf("clusters = %+v, want 1 cluster", clusters)
	}
	want := DupCluster{Indices: []int{1, 3}, Primary: 1} // primary = первый в порядке входа
	if clusters[0].Primary != want.Primary || len(clusters[0].Indices) != 2 ||
		clusters[0].Indices[0] != 1 || clusters[0].Indices[1] != 3 {
		t.Fatalf("clusters[0] = %+v, want %+v", clusters[0], want)
	}
}

func TestOfflineFindDuplicates_DifferentTitles(t *testing.T) {
	o := NewOffline()
	clusters, err := o.FindDuplicates(context.Background(), []DedupItem{
		{Title: "Первая новость"}, {Title: "Вторая новость"}, {Title: "Третья"},
	})
	if err != nil {
		t.Fatalf("FindDuplicates: %v", err)
	}
	if len(clusters) != 0 {
		t.Fatalf("clusters = %+v, want empty (разные заголовки → нет кластеров)", clusters)
	}
}

// TestOfflineFindDuplicates_Deterministic: два вызова на одном входе
// байт-в-байт одинаковы (нет сети/random/time.Now/map-итерации в выходе).
func TestOfflineFindDuplicates_Deterministic(t *testing.T) {
	o := NewOffline()
	items := []DedupItem{
		{Title: "A!"}, {Title: "b"}, {Title: "a"}, {Title: "B…"}, {Title: "c"}, {Title: "a"},
	}
	first, err := o.FindDuplicates(context.Background(), items)
	if err != nil {
		t.Fatalf("FindDuplicates #1: %v", err)
	}
	second, err := o.FindDuplicates(context.Background(), items)
	if err != nil {
		t.Fatalf("FindDuplicates #2: %v", err)
	}
	render := func(cls []DupCluster) string {
		var b strings.Builder
		for _, cl := range cls {
			b.WriteString("cluster:")
			for _, v := range cl.Indices {
				b.WriteString(itoa(v))
				b.WriteString(",")
			}
			b.WriteString("primary=")
			b.WriteString(itoa(cl.Primary))
			b.WriteString(";")
		}
		return b.String()
	}
	if render(first) != render(second) {
		t.Fatalf("non-deterministic: %q vs %q", render(first), render(second))
	}
	// Ожидание: две группы по первому вхождению — {1,3,6} primary 1, {2,4} primary 2.
	if len(first) != 2 || render(first) != "cluster:1,3,6,primary=1;cluster:2,4,primary=2;" {
		t.Fatalf("clusters render = %q, want {1,3,6}/p1 + {2,4}/p2 in first-occurrence order", render(first))
	}
}

// TestClientFindDuplicates_GarbageThenValidSucceeds — G8: первый ответ мусор,
// второй валидный JSON → ровно 2 запроса, кластеры возвращены, err==nil
// (повтор при мусоре не обязан заканчиваться вторым мусором).
func TestClientFindDuplicates_GarbageThenValidSucceeds(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_, _ = w.Write([]byte(chatBody("это мусор, а не JSON")))
			return
		}
		_, _ = w.Write([]byte(chatBody(validDedupJSON)))
	}))
	defer srv.Close()

	c := NewClient("test-key", srv.URL, "test-model", 1)
	clusters, err := c.FindDuplicates(context.Background(), dedupItemsN(4))
	if err != nil {
		t.Fatalf("FindDuplicates: %v", err)
	}
	if len(clusters) != 2 || clusters[0].Primary != 3 || clusters[1].Primary != 2 {
		t.Fatalf("clusters = %+v, want the payload parsed from the second response", clusters)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("server calls = %d, want 2 (мусор → ровно один повтор)", n)
	}
}

// --- G3: формат промпта вызова B ---

// TestBuildDedupPrompt_FormatAndTruncation — нумерация 1-based, title и лид
// ≤200 рун (обрезка на границе), источник + source_id, дата RFC3339 (нулевая
// дата → строки «дата:» нет), пустой Text без лишней строки лида, детерминизм
// (два вызова байт-в-байт), пустой вход.
func TestBuildDedupPrompt_FormatAndTruncation(t *testing.T) {
	longTitle := strings.Repeat("т", 250)
	longText := strings.Repeat("а", 250)
	pub := time.Date(2026, 10, 20, 8, 30, 0, 0, time.UTC)
	items := []DedupItem{
		{Title: longTitle, Text: longText, SourceName: "Src", SourceID: 7, Published: pub},
		{Title: "B", Text: "", SourceName: "S2", SourceID: 8}, // zero Published, пустой лид
	}
	msgs := buildDedupPrompt(items)
	if len(msgs) != 2 || msgs[0].Role != "system" || msgs[0].Content != dedupSystemPrompt {
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
		t.Error("lead не обрезан до 200 рун")
	}
	if !strings.Contains(content, "\nисточник: Src (source_id=7), дата: "+pub.Format(time.RFC3339)) {
		t.Errorf("строка источник/source_id/дата не найдена:\n%s", content)
	}
	if n := strings.Count(content, "дата:"); n != 1 {
		t.Errorf("«дата:» встречается %d раз, want 1 (нулевая Published — без строки даты)", n)
	}
	if !strings.Contains(content, "\n2. B\nисточник: S2 (source_id=8)") {
		t.Errorf("пустой Text дал лишнюю строку лида:\n%s", content)
	}
	// Детерминизм: два вызова на одном входе байт-в-байт.
	again := buildDedupPrompt(items)
	if again[0].Content != msgs[0].Content || again[1].Content != content {
		t.Error("buildDedupPrompt не детерминирован")
	}
	// Пустой вход.
	empty := buildDedupPrompt(nil)
	if len(empty) != 2 || empty[1].Content != "Материалы:\n(недоступны)" {
		t.Errorf("empty-input content = %q, want «Материалы:\\n(недоступны)»", empty[1].Content)
	}
}

func TestNormalizeTitle(t *testing.T) {
	cases := []struct{ in, want string }{
		{"  Go   1.24\tRELEASED! ", "go 1.24 released"},
		{"«Новость…»", "новость"},
		{"...", ""},
		{"", ""},
		{"a-b_c", "a-b_c"}, // внутренние дефис/подчёркивание сохраняются
	}
	for _, c := range cases {
		if got := normalizeTitle(c.in); got != c.want {
			t.Errorf("normalizeTitle(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
