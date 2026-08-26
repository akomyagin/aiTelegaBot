package telegram

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
)

func TestChannelPostToItem(t *testing.T) {
	item := channelPostToItem("mychan", 42, "Hello world\nsecond", 1705309200)

	if item.Kind != "tg_botapi" {
		t.Errorf("Kind = %q, want tg_botapi", item.Kind)
	}
	if item.URL != "https://t.me/mychan/42" {
		t.Errorf("URL = %q", item.URL)
	}
	if item.Title != "Hello world" {
		t.Errorf("Title = %q, want %q", item.Title, "Hello world")
	}
	if got := item.DedupKey(); got != "tg:mychan:42" {
		t.Errorf("DedupKey() = %q, want tg:mychan:42", got)
	}
}

func TestChannelBuffer_Concurrent(t *testing.T) {
	buf := &ChannelBuffer{}
	var wg sync.WaitGroup
	const perGoroutine = 10
	const goroutines = 10 // total pushed (100) stays well under maxBufferedItems
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				buf.Push(feed.Item{Kind: "tg_botapi", Source: "c", ID: "x"})
			}
		}()
	}
	wg.Wait()

	got := buf.Drain()
	want := goroutines * perGoroutine
	if len(got) != want {
		t.Fatalf("Drain() len = %d, want %d", len(got), want)
	}
	if len(buf.Drain()) != 0 {
		t.Errorf("second Drain() should be empty")
	}
}

// TestChannelBuffer_CapsAtMaxBufferedItems confirms Push bounds memory growth:
// once the buffer exceeds maxBufferedItems, the oldest items are dropped so a
// channel with no active drainer cannot grow the buffer without limit.
func TestChannelBuffer_CapsAtMaxBufferedItems(t *testing.T) {
	buf := &ChannelBuffer{}
	total := maxBufferedItems + 50
	for i := 0; i < total; i++ {
		buf.Push(feed.Item{ID: fmt.Sprintf("%d", i)})
	}

	got := buf.Drain()
	if len(got) != maxBufferedItems {
		t.Fatalf("Drain() len = %d, want %d (capped)", len(got), maxBufferedItems)
	}
	// The dropped items should be the OLDEST ones — the newest item pushed
	// (total-1) must be the last one retained.
	if last := got[len(got)-1].ID; last != fmt.Sprintf("%d", total-1) {
		t.Errorf("newest item not retained: last item ID = %q, want %q", last, fmt.Sprintf("%d", total-1))
	}
}

func TestManagedSource_Collect(t *testing.T) {
	buf := &ChannelBuffer{}
	buf.Push(feed.Item{Kind: "tg_botapi", ID: "1"})
	src := NewManagedSource("@chan", buf)

	items, err := src.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	if src.Name() != "@chan" {
		t.Errorf("Name() = %q", src.Name())
	}
}
