package telegram

import (
	"context"
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
	for g := 0; g < 10; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				buf.Push(feed.Item{Kind: "tg_botapi", Source: "c", ID: "x"})
			}
		}()
	}
	wg.Wait()

	got := buf.Drain()
	if len(got) != 1000 {
		t.Fatalf("Drain() len = %d, want 1000", len(got))
	}
	if len(buf.Drain()) != 0 {
		t.Errorf("second Drain() should be empty")
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
