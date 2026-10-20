package telegram

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
)

func TestChannelPostToItem(t *testing.T) {
	item := channelPostToItem("mychan", "mychan", 42, "Hello world\nsecond", 1705309200)

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

// TestChannelPostToItem_PrivateChat covers the key/username split (Stage 8):
// a private chat post has no username, so Source is the numeric key and no
// public t.me URL exists.
func TestChannelPostToItem_PrivateChat(t *testing.T) {
	item := channelPostToItem("-1001234567890", "", 7, "Private post", 1705309200)

	if item.Source != "-1001234567890" {
		t.Errorf("Source = %q, want numeric key", item.Source)
	}
	if item.URL != "" {
		t.Errorf("URL = %q, want empty for private chat", item.URL)
	}
	if got := item.DedupKey(); got != "tg:-1001234567890:7" {
		t.Errorf("DedupKey() = %q", got)
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
				buf.Push("c", feed.Item{Kind: "tg_botapi", Source: "c", ID: "x"})
			}
		}()
	}
	wg.Wait()

	got := buf.Drain("c")
	want := goroutines * perGoroutine
	if len(got) != want {
		t.Fatalf("Drain() len = %d, want %d", len(got), want)
	}
	if len(buf.Drain("c")) != 0 {
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
		buf.Push("k", feed.Item{ID: fmt.Sprintf("%d", i)})
	}

	got := buf.Drain("k")
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
	buf.Push("chan", feed.Item{Kind: "tg_botapi", ID: "1"})
	src := NewManagedSource("@chan", "chan", buf)

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
