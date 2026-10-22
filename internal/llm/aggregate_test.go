// Тесты вызовов C/D (Этап 9D-1, план 9D тест-кейсы 1–2): httptest-паттерн
// classify_test.go (chatBody — общий хелпер helpers_test.go), fallback'и и
// детерминизм Offline. Транспортная ошибка моделируется HTTP 400 (fatal —
// техдолг 9A: retry 429/5xx покрыт общими тестами doWithRetry).

package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
)

// aggSources — детерминированный вход вызова C: два топика, порядок среза.
func aggSources() []SourceBlock {
	return []SourceBlock{
		{SourceID: 11, Name: "Канал A", Summaries: []string{"s1", "s2"}},
		{SourceID: 22, Name: "RSS B", Summaries: []string{"s3", "s4"}},
	}
}

func aggServer(t *testing.T, calls *atomic.Int32, handler func(w http.ResponseWriter)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		handler(w)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func aggJSON(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(chatBody(content)))
}

func TestClientAggregateTopic_ValidJSON(t *testing.T) {
	var calls atomic.Int32
	srv := aggServer(t, &calls, func(w http.ResponseWriter) {
		aggJSON(w, `{"headline":"H","sections":[{"source_index":2,"summary":"s2agg"},{"source_index":1,"summary":"s1agg"}]}`)
	})

	c := NewClient("test-key", srv.URL, "test-model", 1)
	agg, err := c.AggregateTopic(context.Background(), "Тема", aggSources())
	if err != nil {
		t.Fatalf("AggregateTopic: %v", err)
	}
	if agg.Headline != "H" {
		t.Errorf("Headline = %q, want %q", agg.Headline, "H")
	}
	want := []TopicHeadline{{SourceID: 22, Summary: "s2agg"}, {SourceID: 11, Summary: "s1agg"}}
	if !reflect.DeepEqual(agg.Sections, want) {
		t.Errorf("Sections = %+v, want %+v", agg.Sections, want)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("server calls = %d, want 1", n)
	}
}

func TestClientAggregateTopic_FencedJSON(t *testing.T) {
	var calls atomic.Int32
	srv := aggServer(t, &calls, func(w http.ResponseWriter) {
		aggJSON(w, "```json\n{\"headline\":\"H\",\"sections\":[{\"source_index\":1,\"summary\":\"ok\"}]}\n```")
	})

	c := NewClient("test-key", srv.URL, "test-model", 1)
	agg, err := c.AggregateTopic(context.Background(), "Тема", aggSources())
	if err != nil {
		t.Fatalf("AggregateTopic: %v", err)
	}
	if agg.Headline != "H" || len(agg.Sections) != 1 || agg.Sections[0].Summary != "ok" {
		t.Errorf("agg = %+v", agg)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("server calls = %d, want 1 (fence-strip)", n)
	}
}

func TestClientAggregateTopic_IndexValidation(t *testing.T) {
	var calls atomic.Int32
	// source_index 0 и 3 — вне 1..2 → отброшены; дубль 1 → первый выигрыш;
	// пробельный summary → секции нет (деградация L1-строки).
	srv := aggServer(t, &calls, func(w http.ResponseWriter) {
		aggJSON(w, `{"headline":"H","sections":[{"source_index":0,"summary":"a"},{"source_index":3,"summary":"b"},{"source_index":1,"summary":"ok"},{"source_index":1,"summary":"dup"},{"source_index":2,"summary":"   "}]}`)
	})

	c := NewClient("test-key", srv.URL, "test-model", 1)
	agg, err := c.AggregateTopic(context.Background(), "Тема", aggSources())
	if err != nil {
		t.Fatalf("AggregateTopic: %v", err)
	}
	want := []TopicHeadline{{SourceID: 11, Summary: "ok"}}
	if !reflect.DeepEqual(agg.Sections, want) {
		t.Errorf("Sections = %+v, want %+v", agg.Sections, want)
	}
}

func TestClientAggregateTopic_GarbageFallback(t *testing.T) {
	var calls atomic.Int32
	srv := aggServer(t, &calls, func(w http.ResponseWriter) {
		aggJSON(w, "это не json, а prose")
	})

	c := NewClient("test-key", srv.URL, "test-model", 1)
	agg, err := c.AggregateTopic(context.Background(), "Тема", aggSources())
	if err != nil {
		t.Fatalf("двойной мусор — НЕ ошибка (fallback): %v", err)
	}
	// headline = первые ≤3 саммари списком «- …» (порядок среза входа),
	// sections пусты → L1 деградирует на строки без саммари топиков.
	if want := "- s1\n- s2\n- s3"; agg.Headline != want {
		t.Errorf("fallback Headline = %q, want %q", agg.Headline, want)
	}
	if len(agg.Sections) != 0 {
		t.Errorf("fallback Sections = %+v, want empty", agg.Sections)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("server calls = %d, want ровно 2 (один повтор)", n)
	}
}

func TestClientAggregateTopic_EmptyAnswerIsGarbage(t *testing.T) {
	var calls atomic.Int32
	// headline="" И sections=[] при непустом входе — мусор (обобщение решения
	// 9A №8 про {"items":[]}) → повтор → fallback.
	srv := aggServer(t, &calls, func(w http.ResponseWriter) {
		aggJSON(w, `{"headline":"","sections":[]}`)
	})

	c := NewClient("test-key", srv.URL, "test-model", 1)
	agg, err := c.AggregateTopic(context.Background(), "Тема", aggSources())
	if err != nil {
		t.Fatalf("AggregateTopic: %v", err)
	}
	if agg.Headline != "- s1\n- s2\n- s3" || len(agg.Sections) != 0 {
		t.Errorf("agg = %+v, want fallback", agg)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("server calls = %d, want 2", n)
	}
}

func TestClientAggregateTopic_TransportError(t *testing.T) {
	var calls atomic.Int32
	srv := aggServer(t, &calls, func(w http.ResponseWriter) {
		http.Error(w, `{"error":{"message":"bad request"}}`, http.StatusBadRequest)
	})

	c := NewClient("test-key", srv.URL, "test-model", 1)
	if _, err := c.AggregateTopic(context.Background(), "Тема", aggSources()); err == nil {
		t.Fatal("транспортная ошибка — want error")
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("server calls = %d, want 1 (fatal 400 не повторяется)", n)
	}
}

func TestClientAggregateTopic_EmptyInputNoHTTP(t *testing.T) {
	var calls atomic.Int32
	srv := aggServer(t, &calls, func(w http.ResponseWriter) { aggJSON(w, `{}`) })

	c := NewClient("test-key", srv.URL, "test-model", 1)
	agg, err := c.AggregateTopic(context.Background(), "Тема", nil)
	if err != nil || agg.Headline != "" || len(agg.Sections) != 0 {
		t.Fatalf("пустой вход: %+v, %v", agg, err)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("server calls = %d, want 0", n)
	}
}

func TestClientAggregateOverall_ValidJSON(t *testing.T) {
	var calls atomic.Int32
	srv := aggServer(t, &calls, func(w http.ResponseWriter) {
		aggJSON(w, `{"summary":" Общее саммари выпуска. "}`)
	})

	c := NewClient("test-key", srv.URL, "test-model", 1)
	got, err := c.AggregateOverall(context.Background(), []OverallTopic{{Name: "AI", ItemCount: 5, Headline: "h"}})
	if err != nil {
		t.Fatalf("AggregateOverall: %v", err)
	}
	if got != "Общее саммари выпуска." {
		t.Errorf("summary = %q", got)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("server calls = %d, want 1", n)
	}
}

func TestClientAggregateOverall_GarbageFallbackEmpty(t *testing.T) {
	var calls atomic.Int32
	srv := aggServer(t, &calls, func(w http.ResponseWriter) {
		aggJSON(w, "мусор без json")
	})

	c := NewClient("test-key", srv.URL, "test-model", 1)
	got, err := c.AggregateOverall(context.Background(), []OverallTopic{{Name: "AI", ItemCount: 5, Headline: "h"}})
	// Решение 9D-1 (§4.8.1 шаг 6): двойной мусор → НЕ ошибка, overall "".
	if err != nil {
		t.Fatalf("двойной мусор D — want (\"\", nil): %v", err)
	}
	if got != "" {
		t.Errorf("summary = %q, want \"\"", got)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("server calls = %d, want ровно 2", n)
	}
}

func TestClientAggregateOverall_TransportError(t *testing.T) {
	var calls atomic.Int32
	srv := aggServer(t, &calls, func(w http.ResponseWriter) {
		http.Error(w, `{"error":{"message":"bad request"}}`, http.StatusBadRequest)
	})

	c := NewClient("test-key", srv.URL, "test-model", 1)
	if _, err := c.AggregateOverall(context.Background(), []OverallTopic{{Name: "AI"}}); err == nil {
		t.Fatal("транспортная ошибка D — want error (вызывающий 9D-2 log-only)")
	}
}

func TestOfflineAggregateTopic_Deterministic(t *testing.T) {
	o := NewOffline()
	ctx := context.Background()
	a, err := o.AggregateTopic(ctx, "Тема", aggSources())
	if err != nil {
		t.Fatalf("AggregateTopic: %v", err)
	}
	// §4.7: топик/тема — первые ≤3 строки «- <саммари>».
	if want := "- s1\n- s2\n- s3"; a.Headline != want {
		t.Errorf("Headline = %q, want %q", a.Headline, want)
	}
	want := []TopicHeadline{
		{SourceID: 11, Summary: "- s1\n- s2"},
		{SourceID: 22, Summary: "- s3\n- s4"},
	}
	if !reflect.DeepEqual(a.Sections, want) {
		t.Errorf("Sections = %+v, want %+v", a.Sections, want)
	}
	b, _ := o.AggregateTopic(ctx, "Тема", aggSources())
	if !reflect.DeepEqual(a, b) {
		t.Errorf("не детерминированно:\n%+v\n%+v", a, b)
	}
}

func TestOfflineAggregateOverall_Deterministic(t *testing.T) {
	o := NewOffline()
	ctx := context.Background()
	topics := []OverallTopic{
		{Name: "AI", ItemCount: 5, Headline: "h"},
		{Name: "  ", ItemCount: 2},
	}
	want := "- AI: 5 материалов\n- Без темы: 2 материалов"
	a, err := o.AggregateOverall(ctx, topics)
	if err != nil {
		t.Fatalf("AggregateOverall: %v", err)
	}
	if a != want {
		t.Errorf("overall = %q, want %q", a, want)
	}
	b, _ := o.AggregateOverall(ctx, topics)
	if a != b {
		t.Errorf("не детерминированно: %q vs %q", a, b)
	}
	// Пустой вход → "" без падения.
	if e, err := o.AggregateOverall(ctx, nil); e != "" || err != nil {
		t.Errorf("пустой вход: %q, %v", e, err)
	}
}
