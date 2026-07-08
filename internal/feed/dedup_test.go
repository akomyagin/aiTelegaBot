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
		{"tg_botapi by id", feed.Item{Kind: "tg_botapi", Source: "chan", ID: "42"}, "tg:chan:42"},
		{"tg_public by id", feed.Item{Kind: "tg_public", Source: "chan", ID: "7"}, "tg:chan:7"},
		{"tg_public without id falls back to url", feed.Item{Kind: "tg_public", URL: "https://x.com"}, "https://x.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.item.DedupKey(); got != tt.want {
				t.Errorf("DedupKey() = %q, want %q", got, tt.want)
			}
		})
	}
}
