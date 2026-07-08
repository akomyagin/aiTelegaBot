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

	"github.com/akomyagin/aiTelegaBot/internal/feed"
)

const okBody = `{"choices":[{"message":{"content":"итог"}}]}`

func testItems() []feed.Item {
	return []feed.Item{{Title: "T", URL: "http://x", Text: "body"}}
}

func TestRetry_429ThenSuccess(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(okBody))
	}))
	defer srv.Close()

	c := NewClient("key", srv.URL, "m", 3)
	out, err := c.Summarize(context.Background(), testItems())
	if err != nil {
		t.Fatal(err)
	}
	if out != "итог" {
		t.Fatalf("content = %q", out)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("want 2 requests, got %d", n)
	}
}

func TestRetry_FatalError(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := NewClient("key", srv.URL, "m", 3)
	_, err := c.Summarize(context.Background(), testItems())
	if err == nil {
		t.Fatal("want error on 401")
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("fatal error should not retry, got %d requests", n)
	}
}

func TestRetry_NetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	url := srv.URL
	srv.Close() // closed before use => connection refused

	c := NewClient("key", url, "m", 2)
	_, err := c.Summarize(context.Background(), testItems())
	if err == nil {
		t.Fatal("want network error")
	}
}

func TestNoKeyInLogs(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(orig)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	const secret = "SECRET_KEY_123"
	c := NewClient(secret, srv.URL, "m", 2)
	_, _ = c.Summarize(context.Background(), testItems())

	if strings.Contains(buf.String(), secret) {
		t.Fatalf("api key leaked into logs: %s", buf.String())
	}
}
