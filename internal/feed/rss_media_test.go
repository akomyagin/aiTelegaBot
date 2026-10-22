package feed_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
)

// TestRSSSourceCollect_Media (Stage 9E, тест-кейс №1): enclosure image/* →
// MediaKind=photo + MediaURL; enclosure application/pdf и пост без enclosure →
// media-поля пустые.
func TestRSSSourceCollect_Media(t *testing.T) {
	sample, err := os.ReadFile("testdata/rss_photo.xml")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write(sample)
	}))
	defer srv.Close()

	src := feed.NewRSSSource("photofeed", srv.URL, "rss", srv.Client())
	items, err := src.Collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("want 3 items, got %d", len(items))
	}

	if items[0].MediaKind != "photo" {
		t.Errorf("image enclosure: MediaKind = %q, want photo", items[0].MediaKind)
	}
	if want := "https://example.com/pic.jpg"; items[0].MediaURL != want {
		t.Errorf("image enclosure: MediaURL = %q, want %q", items[0].MediaURL, want)
	}
	if items[1].MediaKind != "" || items[1].MediaURL != "" {
		t.Errorf("pdf enclosure: media-поля должны быть пустыми, got %q/%q",
			items[1].MediaKind, items[1].MediaURL)
	}
	if items[2].MediaKind != "" || items[2].MediaURL != "" {
		t.Errorf("no enclosure: media-поля должны быть пустыми, got %q/%q",
			items[2].MediaKind, items[2].MediaURL)
	}
}
