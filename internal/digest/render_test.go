package digest

import (
	"testing"
	"time"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
)

func TestRender(t *testing.T) {
	now := time.Date(2024, 1, 15, 9, 0, 0, 0, time.UTC)
	items := []feed.Item{
		{Title: "First"},
		{Title: "Second"},
	}
	summary := "- First item\n- Second item"

	got := Render(summary, items, now)
	want := "📋 Дайджест — 15 Jan 2024\n\n- First item\n- Second item\n\n— 2 материалов"

	if got != want {
		t.Errorf("Render mismatch:\n got: %q\nwant: %q", got, want)
	}
}
