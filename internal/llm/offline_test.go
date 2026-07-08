package llm

import (
	"context"
	"strings"
	"testing"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
)

func TestOfflineSummarize_Empty(t *testing.T) {
	out, err := NewOffline().Summarize(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if out != "Нет новых материалов." {
		t.Fatalf("got %q", out)
	}
}

func TestOfflineSummarize_Deterministic(t *testing.T) {
	items := []feed.Item{
		{Title: "A", URL: "http://a", Text: "Первое предложение. Второе."},
		{Title: "B", URL: "http://b", Text: "Что-то."},
	}
	o := NewOffline()
	out1, _ := o.Summarize(context.Background(), items)
	out2, _ := o.Summarize(context.Background(), items)
	if out1 != out2 {
		t.Fatalf("non-deterministic:\n%q\n%q", out1, out2)
	}
}

func TestOfflineSummarize_NoText(t *testing.T) {
	items := []feed.Item{{Title: "Title", URL: "http://x"}}
	out, _ := NewOffline().Summarize(context.Background(), items)
	if !strings.Contains(out, "Title") || !strings.Contains(out, "http://x") {
		t.Fatalf("missing title/url: %q", out)
	}
}

func TestOfflineSummarize_LeadTruncated(t *testing.T) {
	long := strings.Repeat("я", leadRunes+300) // no ". " boundary
	items := []feed.Item{{Title: "T", URL: "http://x", Text: long}}
	out, _ := NewOffline().Summarize(context.Background(), items)
	// The lead sits between ": " and " (". Extract and count runes.
	start := strings.Index(out, ": ")
	end := strings.LastIndex(out, " (")
	if start < 0 || end < 0 || end <= start {
		t.Fatalf("unexpected format: %q", out)
	}
	lead := out[start+2 : end]
	if got := len([]rune(lead)); got > leadRunes {
		t.Fatalf("lead %d runes, want <= %d", got, leadRunes)
	}
}

func TestOfflineSummarize_Order(t *testing.T) {
	items := []feed.Item{
		{Title: "First", URL: "http://one"},
		{Title: "Second", URL: "http://two"},
	}
	out, _ := NewOffline().Summarize(context.Background(), items)
	if strings.Index(out, "First") >= strings.Index(out, "Second") {
		t.Fatalf("order not preserved: %q", out)
	}
}
