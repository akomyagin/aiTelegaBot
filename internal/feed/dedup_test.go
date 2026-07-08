package feed_test

import (
	"testing"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
)

func TestItemDedupKey(t *testing.T) {
	tests := []struct {
		name string
		item feed.Item
		want string
	}{
		{"url present", feed.Item{URL: "https://x.com"}, "https://x.com"},
		{"no url falls back to kind:title", feed.Item{Kind: "hn", Title: "foo"}, "hn:foo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.item.DedupKey(); got != tt.want {
				t.Errorf("DedupKey() = %q, want %q", got, tt.want)
			}
		})
	}
}
