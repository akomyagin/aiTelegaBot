package llm

import (
	"strings"
	"testing"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
)

func TestBuildPrompt_Empty(t *testing.T) {
	msgs := buildPrompt(nil)
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages, got %d", len(msgs))
	}
	if msgs[0].Role != "system" || msgs[1].Role != "user" {
		t.Fatalf("roles: %q, %q", msgs[0].Role, msgs[1].Role)
	}
	if msgs[1].Content != "" {
		t.Fatalf("empty items should give empty user content, got %q", msgs[1].Content)
	}
}

func TestBuildPrompt_TruncatesText(t *testing.T) {
	long := strings.Repeat("я", maxTextRunes+300)
	msgs := buildPrompt([]feed.Item{{Title: "T", URL: "http://x", Text: long}})
	user := msgs[1].Content
	// Count runes of the body portion: everything after the URL line.
	idx := strings.Index(user, "http://x\n")
	if idx < 0 {
		t.Fatal("URL line missing")
	}
	body := user[idx+len("http://x\n"):]
	if got := len([]rune(body)); got != maxTextRunes {
		t.Fatalf("body truncated to %d runes, want %d", got, maxTextRunes)
	}
}

func TestBuildPrompt_OrderAndFields(t *testing.T) {
	items := []feed.Item{
		{Title: "First", URL: "http://one"},
		{Title: "Second", URL: "http://two"},
	}
	user := buildPrompt(items)[1].Content
	i1 := strings.Index(user, "First")
	i2 := strings.Index(user, "Second")
	if i1 < 0 || i2 < 0 {
		t.Fatalf("titles missing: %q", user)
	}
	if i1 >= i2 {
		t.Fatalf("order not preserved: %q", user)
	}
	if !strings.Contains(user, "http://one") || !strings.Contains(user, "http://two") {
		t.Fatalf("URLs missing: %q", user)
	}
	if !strings.Contains(user, "1. First") {
		t.Fatalf("numbering missing: %q", user)
	}
}
