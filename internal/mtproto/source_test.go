package mtproto

import (
	"testing"
	"time"

	"github.com/gotd/td/tg"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
)

func TestMessageToFeedItem(t *testing.T) {
	msg := tg.Message{
		ID:      42,
		Message: "Breaking headline\nBody text on the second line.",
		Date:    1700000000,
	}
	item := messageToFeedItem(msg, "somechannel")

	if item.Kind != "tg_mtproto" {
		t.Errorf("Kind = %q, want tg_mtproto", item.Kind)
	}
	if item.Source != "somechannel" {
		t.Errorf("Source = %q, want somechannel", item.Source)
	}
	if item.ID != "42" {
		t.Errorf("ID = %q, want 42", item.ID)
	}
	if item.Title != "Breaking headline" {
		t.Errorf("Title = %q, want first line only", item.Title)
	}
	if item.Text != msg.Message {
		t.Errorf("Text = %q, want full message", item.Text)
	}
	if item.URL != "" {
		t.Errorf("URL = %q, want empty for private channel", item.URL)
	}
	want := time.Unix(1700000000, 0).UTC()
	if !item.Published.Equal(want) {
		t.Errorf("Published = %v, want %v", item.Published, want)
	}

	// DedupKey uses the tg: scheme (see feed.Item.DedupKey).
	if got, want := item.DedupKey(), "tg:somechannel:42"; got != want {
		t.Errorf("DedupKey = %q, want %q", got, want)
	}
}

// Ensure ChannelSource satisfies feed.Source at compile time.
var _ feed.Source = (*ChannelSource)(nil)

func TestFirstLine(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"single line", "hello world", "hello world"},
		{"multiline", "first\nsecond\nthird", "first"},
		{"leading whitespace", "  \n trimmed", "trimmed"},
		{"empty", "", ""},
		{"truncates to 100 runes", string(make([]rune, 150)), string(make([]rune, 100))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := firstLine(tt.in); got != tt.want {
				t.Errorf("firstLine(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
