package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestPublicSource_OK(t *testing.T) {
	fixture, err := os.ReadFile("testdata/tme_channel.html")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(fixture)
	}))
	defer srv.Close()

	ps := NewPublicSource("@testchan", "testchan", srv.Client(), 20)
	ps.SetBaseURL(srv.URL)

	items, err := ps.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
	if items[0].ID != "101" {
		t.Errorf("items[0].ID = %q, want 101", items[0].ID)
	}
	if items[0].Kind != "tg_public" {
		t.Errorf("items[0].Kind = %q, want tg_public", items[0].Kind)
	}
	if items[0].Title != "First post" {
		t.Errorf("items[0].Title = %q, want %q", items[0].Title, "First post")
	}
	if got := items[0].DedupKey(); got != "tg:testchan:101" {
		t.Errorf("DedupKey() = %q, want tg:testchan:101", got)
	}
	if items[0].URL != "https://t.me/testchan/101" {
		t.Errorf("items[0].URL = %q", items[0].URL)
	}
	if items[0].Published.IsZero() {
		t.Errorf("items[0].Published should be parsed, got zero")
	}
}

func TestPublicSource_Degradation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ps := NewPublicSource("@testchan", "testchan", srv.Client(), 20)
	ps.SetBaseURL(srv.URL)

	items, err := ps.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect should degrade gracefully, got err %v", err)
	}
	if len(items) != 0 {
		t.Errorf("got %d items, want 0 on error", len(items))
	}
}

func TestPublicSource_LimitApplied(t *testing.T) {
	fixture, err := os.ReadFile("testdata/tme_channel.html")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(fixture)
	}))
	defer srv.Close()

	ps := NewPublicSource("@testchan", "testchan", srv.Client(), 1)
	ps.SetBaseURL(srv.URL)

	items, err := ps.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1 (limit)", len(items))
	}
}
