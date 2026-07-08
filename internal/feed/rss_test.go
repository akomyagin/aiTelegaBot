package feed_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
)

func TestRSSSourceCollect(t *testing.T) {
	sample, err := os.ReadFile("testdata/rss_sample.xml")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write(sample)
	}))
	defer srv.Close()

	src := feed.NewRSSSource("test", srv.URL, "rss", srv.Client())
	items, err := src.Collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("want 2 items, got %d", len(items))
	}
	if items[0].Title != "Test Item 1" {
		t.Errorf("title = %q", items[0].Title)
	}
	if items[0].URL != "https://example.com/1" {
		t.Errorf("url = %q", items[0].URL)
	}
	if items[0].Kind != "rss" {
		t.Errorf("kind = %q", items[0].Kind)
	}
	if items[0].Source != "test" {
		t.Errorf("source = %q", items[0].Source)
	}
	if items[0].Published.IsZero() {
		t.Errorf("published not parsed: %v", items[0].Published)
	}
}
