package feed_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
)

func TestHNSourceCollect(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v0/topstories.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[1,2,3]`))
	})
	mux.HandleFunc("/v0/item/1.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":1,"title":"Story 1","url":"https://hn.test/1","time":1704067200,"type":"story"}`))
	})
	mux.HandleFunc("/v0/item/2.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":2,"title":"Ask HN","time":1704067200,"type":"story"}`))
	})
	mux.HandleFunc("/v0/item/3.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":3,"title":"Dead","time":1704067200,"type":"story","dead":true}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	src := feed.NewHNSource("HN", 3, srv.Client())
	src.SetBaseURL(srv.URL + "/v0")

	items, err := src.Collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("want 2 items (dead filtered), got %d", len(items))
	}
	// items[0] = Story 1, items[1] = Ask HN (no url → HN permalink).
	if !strings.Contains(items[1].URL, "news.ycombinator.com") {
		t.Errorf("ask-hn url = %q, want HN permalink", items[1].URL)
	}
	if items[0].Kind != "hn" {
		t.Errorf("kind = %q", items[0].Kind)
	}
}
