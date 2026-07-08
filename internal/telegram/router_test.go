package telegram

import "testing"

func TestParseCommand(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		wantCmd string
		wantOK  bool
	}{
		{"plain", "/start", "start", true},
		{"botname suffix", "/help@MyBot", "help", true},
		{"with arg", "/digest arg", "digest", true},
		{"uppercase", "/SOURCES", "sources", true},
		{"not a command", "hello", "", false},
		{"empty", "", "", false},
		{"bare slash", "/", "", false},
		{"leading space", "  /start", "start", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, ok := parseCommand(tt.text)
			if cmd != tt.wantCmd || ok != tt.wantOK {
				t.Fatalf("parseCommand(%q) = (%q, %v), want (%q, %v)",
					tt.text, cmd, ok, tt.wantCmd, tt.wantOK)
			}
		})
	}
}

func TestAuthorized(t *testing.T) {
	tests := []struct {
		name     string
		incoming int64
		owner    int64
		want     bool
	}{
		{"match", 123, 123, true},
		{"mismatch", 456, 123, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := authorized(tt.incoming, tt.owner); got != tt.want {
				t.Fatalf("authorized(%d, %d) = %v, want %v",
					tt.incoming, tt.owner, got, tt.want)
			}
		})
	}
}
