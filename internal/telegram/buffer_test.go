package telegram

import (
	"fmt"
	"testing"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
)

// TestChannelBuffer_KeyIsolation is the Stage 8 regression test for the
// "first drainer steals all posts" tech debt: draining one key must not touch
// the items buffered under another key.
func TestChannelBuffer_KeyIsolation(t *testing.T) {
	buf := &ChannelBuffer{}
	buf.Push("alpha", feed.Item{Source: "alpha", ID: "1"})
	buf.Push("alpha", feed.Item{Source: "alpha", ID: "2"})
	buf.Push("beta", feed.Item{Source: "beta", ID: "3"})

	got := buf.Drain("alpha")
	if len(got) != 2 {
		t.Fatalf("Drain(alpha) len = %d, want 2", len(got))
	}
	for _, it := range got {
		if it.Source != "alpha" {
			t.Errorf("Drain(alpha) returned item of %q", it.Source)
		}
	}
	if len(buf.Drain("alpha")) != 0 {
		t.Errorf("second Drain(alpha) should be empty")
	}

	beta := buf.Drain("beta")
	if len(beta) != 1 || beta[0].ID != "3" {
		t.Fatalf("Drain(beta) = %+v, want the single beta item untouched by Drain(alpha)", beta)
	}
}

// TestChannelBuffer_PerKeyCap verifies the maxBufferedItems cap is applied per
// key: an unread channel cannot evict items of another channel.
func TestChannelBuffer_PerKeyCap(t *testing.T) {
	buf := &ChannelBuffer{}
	buf.Push("quiet", feed.Item{Source: "quiet", ID: "keep"})

	total := maxBufferedItems + 50
	for i := 0; i < total; i++ {
		buf.Push("loud", feed.Item{Source: "loud", ID: fmt.Sprintf("%d", i)})
	}

	loud := buf.Drain("loud")
	if len(loud) != maxBufferedItems {
		t.Fatalf("Drain(loud) len = %d, want %d (per-key cap)", len(loud), maxBufferedItems)
	}
	if last := loud[len(loud)-1].ID; last != fmt.Sprintf("%d", total-1) {
		t.Errorf("newest loud item not retained: last ID = %q, want %q", last, fmt.Sprintf("%d", total-1))
	}

	quiet := buf.Drain("quiet")
	if len(quiet) != 1 || quiet[0].ID != "keep" {
		t.Fatalf("Drain(quiet) = %+v, want the untouched quiet item", quiet)
	}
}
