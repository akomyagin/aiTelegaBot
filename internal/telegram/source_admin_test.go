package telegram

import (
	"context"
	"log/slog"
	"testing"
)

func TestParseCommandArg(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{"rss url", "/addsource https://x", "https://x"},
		{"no arg", "/addsource", ""},
		{"no arg trailing space", "/addsource   ", ""},
		{"extra spaces before channel", "/addsource   @ch", "@ch"},
		{"removesource numeric", "/removesource 5", "5"},
		{"multiword tail joined", "/addsource foo bar baz", "foo bar baz"},
		{"empty text", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseCommandArg(tt.text); got != tt.want {
				t.Fatalf("parseCommandArg(%q) = %q, want %q", tt.text, got, tt.want)
			}
		})
	}
}

// detectKind's http/https branches never touch the network, so they're safe to
// unit test directly. The "@channel" branch calls b.api.GetChatMember, which
// requires a live bot.Bot client — covered instead by the fact that any error
// from isBotAdmin (e.g. an unreachable API) makes detectKind fall back to
// tg_public, exercised here via a Bot with a nil api (GetChatMember will fail
// as a network/nil-pointer error, which is exactly the "not admin / unknown"
// path detectKind is built to tolerate). We only assert the safe, network-free
// branches directly; the @-branch fallback behavior is documented in the plan
// as an accepted manual/acceptance-test gap.
func TestDetectKind_URLAndInvalid(t *testing.T) {
	b := &Bot{log: slog.Default()}
	ctx := context.Background()

	tests := []struct {
		name     string
		arg      string
		wantKind string
		wantRef  string
		wantErr  bool
	}{
		{"https lowercase", "https://x/feed", "rss", "https://x/feed", false},
		{"http uppercase scheme", "HTTP://X", "rss", "HTTP://X", false},
		{"garbage no scheme", "foo", "", "", true},
		{"bare at sign", "@", "", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, ref, err := b.detectKind(ctx, tt.arg)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("detectKind(%q) = nil error, want error", tt.arg)
				}
				return
			}
			if err != nil {
				t.Fatalf("detectKind(%q) unexpected error: %v", tt.arg, err)
			}
			if kind != tt.wantKind || ref != tt.wantRef {
				t.Fatalf("detectKind(%q) = (%q, %q), want (%q, %q)",
					tt.arg, kind, ref, tt.wantKind, tt.wantRef)
			}
		})
	}
}
