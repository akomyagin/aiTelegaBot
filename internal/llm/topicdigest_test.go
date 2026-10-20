package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
)

// chatBody wraps content into an OpenAI-compatible chat-completions response.
func chatBody(content string) string {
	return `{"choices":[{"message":{"content":` + strconv.Quote(content) + `}}]}`
}

func topicDigestItems(n int) []feed.Item {
	items := make([]feed.Item, n)
	for i := range items {
		items[i] = feed.Item{
			Title: "T" + strconv.Itoa(i+1),
			Text:  "text " + strconv.Itoa(i+1),
			URL:   "https://e.example/" + strconv.Itoa(i+1),
		}
	}
	return items
}

const validTopicJSON = `{"headline":"Обзор","items":[{"index":1,"summary":"s1"},{"index":2,"summary":"s2"}]}`

func TestClientSummarizeTopic_ValidJSON(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatBody(validTopicJSON)))
	}))
	defer srv.Close()

	c := NewClient("test-key", srv.URL, "test-model", 1)
	ts, err := c.SummarizeTopic(context.Background(), "Тема", topicDigestItems(2))
	if err != nil {
		t.Fatalf("SummarizeTopic: %v", err)
	}
	if ts.Headline != "Обзор" {
		t.Errorf("Headline = %q, want «Обзор»", ts.Headline)
	}
	want := []ItemSummary{{Index: 1, Summary: "s1"}, {Index: 2, Summary: "s2"}}
	if len(ts.Items) != 2 || ts.Items[0] != want[0] || ts.Items[1] != want[1] {
		t.Errorf("Items = %+v, want %+v", ts.Items, want)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("server calls = %d, want 1", n)
	}
}

func TestClientSummarizeTopic_FencedJSON(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatBody("```json\n" + validTopicJSON + "\n```")))
	}))
	defer srv.Close()

	c := NewClient("test-key", srv.URL, "test-model", 1)
	ts, err := c.SummarizeTopic(context.Background(), "Тема", topicDigestItems(2))
	if err != nil {
		t.Fatalf("SummarizeTopic over fenced JSON: %v", err)
	}
	if ts.Headline != "Обзор" || len(ts.Items) != 2 {
		t.Errorf("ts = %+v, want the parsed payload", ts)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("server calls = %d, want 1", n)
	}
}

func TestClientSummarizeTopic_FallbackToSummarize(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n <= 2 {
			// Unparseable answers for both SummarizeTopic attempts.
			_, _ = w.Write([]byte(chatBody("это не JSON, а обычный текст")))
			return
		}
		// The third call is the fallback Summarize.
		_, _ = w.Write([]byte(chatBody("FALLBACK HEADLINE")))
	}))
	defer srv.Close()

	c := NewClient("test-key", srv.URL, "test-model", 1)
	ts, err := c.SummarizeTopic(context.Background(), "Тема", topicDigestItems(2))
	if err != nil {
		t.Fatalf("SummarizeTopic with fallback: %v", err)
	}
	if ts.Headline != "FALLBACK HEADLINE" {
		t.Errorf("Headline = %q, want the fallback Summarize result", ts.Headline)
	}
	if len(ts.Items) != 0 {
		t.Errorf("Items = %+v, want empty (degradation, not failure)", ts.Items)
	}
	if n := calls.Load(); n != 3 {
		t.Errorf("server calls = %d, want exactly 3 (2 parse attempts + 1 fallback)", n)
	}
}

func TestClientSummarizeTopic_FallbackFails(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n <= 2 {
			_, _ = w.Write([]byte(chatBody("мусор")))
			return
		}
		// The fallback Summarize hits a fatal 400 — no re-request.
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad request"}}`))
	}))
	defer srv.Close()

	c := NewClient("test-key", srv.URL, "test-model", 1)
	if _, err := c.SummarizeTopic(context.Background(), "Тема", topicDigestItems(2)); err == nil {
		t.Fatal("SummarizeTopic succeeded, want error (garbage + failing fallback)")
	}
	if n := calls.Load(); n != 3 {
		t.Errorf("server calls = %d, want 3 (400 is fatal — not retried)", n)
	}
}

func TestParseTopicSummary(t *testing.T) {
	tests := []struct {
		name         string
		content      string
		n            int
		wantHeadline string
		wantItems    []ItemSummary
		wantErr      bool
	}{
		{
			name: "valid", content: `{"headline":" H ","items":[{"index":1,"summary":"a"}]}`, n: 1,
			wantHeadline: "H", wantItems: []ItemSummary{{Index: 1, Summary: "a"}},
		},
		{
			name:         "out-of-range indexes and duplicates dropped, first wins",
			content:      `{"headline":"H","items":[{"index":1,"summary":"a"},{"index":9,"summary":"x"},{"index":1,"summary":"b"},{"index":2,"summary":"d"},{"index":0,"summary":"z"}]}`,
			n:            2,
			wantHeadline: "H", wantItems: []ItemSummary{{Index: 1, Summary: "a"}, {Index: 2, Summary: "d"}},
		},
		{
			name:    "missing indexes are not an error",
			content: `{"headline":"H","items":[{"index":2,"summary":"only"}]}`, n: 3,
			wantHeadline: "H", wantItems: []ItemSummary{{Index: 2, Summary: "only"}},
		},
		{
			name: "empty headline is not an error", content: `{"headline":"","items":[]}`, n: 2,
			wantHeadline: "", wantItems: nil,
		},
		{
			name: "fenced", content: "```json\n{\"headline\":\"H\",\"items\":[]}\n```", n: 1,
			wantHeadline: "H", wantItems: nil,
		},
		{name: "garbage", content: "not json at all", n: 1, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts, err := parseTopicSummary(tt.content, tt.n)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseTopicSummary(%q) err=nil, want error", tt.content)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTopicSummary: %v", err)
			}
			if ts.Headline != tt.wantHeadline {
				t.Errorf("Headline = %q, want %q", ts.Headline, tt.wantHeadline)
			}
			if len(ts.Items) != len(tt.wantItems) {
				t.Fatalf("Items = %+v, want %+v", ts.Items, tt.wantItems)
			}
			for i := range ts.Items {
				if ts.Items[i] != tt.wantItems[i] {
					t.Errorf("Items[%d] = %+v, want %+v", i, ts.Items[i], tt.wantItems[i])
				}
			}
		})
	}

	t.Run("headline cut to 800 runes", func(t *testing.T) {
		long := strings.Repeat("ы", 900)
		ts, err := parseTopicSummary(`{"headline":"`+long+`","items":[]}`, 0)
		if err != nil {
			t.Fatalf("parseTopicSummary: %v", err)
		}
		if r := []rune(ts.Headline); len(r) != 800 {
			t.Errorf("headline = %d runes, want 800", len(r))
		}
	})
}

func TestOfflineSummarizeTopic(t *testing.T) {
	items := []feed.Item{
		{Title: "A", Text: "First sentence. Second one."},
		{Title: "B", Text: ""},
		{Title: "C", Text: "Just text"},
		{Title: "D", Text: "Fourth item"},
	}
	o := NewOffline()
	ctx := context.Background()

	ts1, err := o.SummarizeTopic(ctx, "Тема", items)
	if err != nil {
		t.Fatalf("SummarizeTopic: %v", err)
	}
	ts2, err := o.SummarizeTopic(ctx, "Тема", items)
	if err != nil {
		t.Fatalf("SummarizeTopic 2: %v", err)
	}
	// Determinism: two calls byte-for-byte.
	b1, _ := json.Marshal(ts1)
	b2, _ := json.Marshal(ts2)
	if string(b1) != string(b2) {
		t.Errorf("offline is not deterministic:\n %s\n %s", b1, b2)
	}

	if want := "- A\n- B\n- C"; ts1.Headline != want {
		t.Errorf("Headline = %q, want %q (up to 3 first «- Title» lines)", ts1.Headline, want)
	}
	want := []ItemSummary{
		{Index: 1, Summary: "First sentence."},
		{Index: 2, Summary: "B"}, // empty Text → Title
		{Index: 3, Summary: "Just text"},
		{Index: 4, Summary: "Fourth item"},
	}
	if len(ts1.Items) != len(want) {
		t.Fatalf("Items = %+v, want %+v", ts1.Items, want)
	}
	for i := range want {
		if ts1.Items[i] != want[i] {
			t.Errorf("Items[%d] = %+v, want %+v", i, ts1.Items[i], want[i])
		}
	}
}
