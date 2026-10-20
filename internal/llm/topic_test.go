package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// Both implementations satisfy the interface (plan §4.10).
var (
	_ TopicSuggester = (*Client)(nil)
	_ TopicSuggester = (*Offline)(nil)
)

// chatJSON builds a chat-completions response body carrying content.
func chatJSON(t *testing.T, content string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"choices": []map[string]any{{"message": map[string]any{"content": content}}},
	})
	if err != nil {
		t.Fatalf("marshal chat body: %v", err)
	}
	return string(b)
}

// TestSuggestTopic covers the plan's Stage-B llm cases: valid JSON, JSON in
// ```-fences, garbage → exactly two requests → error, an answer not in
// existing → accepted as a new name, overlong name truncated to 40 runes.
func TestSuggestTopic(t *testing.T) {
	long := strings.Repeat("а", 50)
	tests := []struct {
		name      string
		content   string
		existing  []string
		want      string
		wantCalls int32
		wantErr   bool
	}{
		{"valid json", `{"topic":"Новости"}`, []string{"Новости"}, "Новости", 1, false},
		{"fenced json", "```json\n{\"topic\":\"AI\"}\n```", nil, "AI", 1, false},
		{"plain fence", "```\n{\"topic\":\"AI\"}\n```", nil, "AI", 1, false},
		{"new name not in existing", `{"topic":"Техника"}`, []string{"Новости"}, "Техника", 1, false},
		{"empty topic = nothing to suggest", `{"topic":""}`, nil, "", 1, false},
		{"overlong name truncated to 40 runes", `{"topic":"` + long + `"}`, nil, strings.Repeat("а", 40), 1, false},
		{"garbage: one re-request then error", "извини, я не могу это сделать", nil, "", 2, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			respBody := chatJSON(t, tt.content)
			var calls int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt32(&calls, 1)
				_, _ = w.Write([]byte(respBody))
			}))
			defer srv.Close()

			c := NewClient("secret-key", srv.URL, "m", 3)
			got, err := c.SuggestTopic(context.Background(), tt.existing, testItems())
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error, got name %q", got)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("name = %q, want %q", got, tt.want)
			}
			if n := atomic.LoadInt32(&calls); n != tt.wantCalls {
				t.Errorf("server calls = %d, want %d", n, tt.wantCalls)
			}
		})
	}
}

// TestSuggestTopic_FatalHTTPErrorNoReparse: a fatal API error surfaces from the
// first attempt without the parse-retry doubling the requests.
func TestSuggestTopic_FatalHTTPErrorNoReparse(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := NewClient("secret-key", srv.URL, "m", 3)
	if _, err := c.SuggestTopic(context.Background(), nil, testItems()); err == nil {
		t.Fatal("want error on 401")
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("fatal error should not re-request, got %d calls", n)
	}
}

// TestSuggestTopic_Offline: the offline implementation deterministically
// suggests nothing.
func TestSuggestTopic_Offline(t *testing.T) {
	got, err := NewOffline().SuggestTopic(context.Background(), []string{"Новости"}, testItems())
	if err != nil {
		t.Fatalf("offline SuggestTopic: %v", err)
	}
	if got != "" {
		t.Errorf("offline suggestion = %q, want empty", got)
	}
}
