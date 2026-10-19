package telegram

import "testing"

func TestParseTmeLink(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		wantOK   bool // false — not a t.me link at all
		wantKind string
		wantUser string
		wantID   int64
	}{
		{"public no scheme", "t.me/golang_news", true, "public", "golang_news", 0},
		{"public https", "https://t.me/golang_news", true, "public", "golang_news", 0},
		{"public http", "http://t.me/golang_news", true, "public", "golang_news", 0},
		{"public telegram.me", "telegram.me/durov", true, "public", "durov", 0},
		{"public s prefix", "t.me/s/golang_news", true, "public", "golang_news", 0},
		{"public s prefix with msg", "https://t.me/s/golang_news/123", true, "public", "golang_news", 0},
		{"public message link", "t.me/golang_news/123", true, "public", "golang_news", 0},
		{"public extra path", "t.me/golang_news/123/extra", true, "public", "golang_news", 0},
		{"public query dropped", "https://t.me/golang_news?comment=1", true, "public", "golang_news", 0},
		{"public host case", "https://T.ME/golang_news", true, "public", "golang_news", 0},
		{"private c", "t.me/c/1234567890", true, "private", "", -1001234567890},
		{"private c with msg", "https://t.me/c/1234567890/55", true, "private", "", -1001234567890},
		{"private c small id", "t.me/c/1", true, "private", "", -1000000000001},
		{"private c max valid", "t.me/c/999999999999", true, "private", "", -1999999999999},
		{"c id at offset bound", "t.me/c/1000000000000", true, "unsupported", "", 0},
		{"c id max int64", "t.me/c/9223372036854775807", true, "unsupported", "", 0},
		{"invite plus", "t.me/+ABCdef123456", true, "unsupported", "", 0},
		{"invite joinchat", "t.me/joinchat/AAAAAEf", true, "unsupported", "", 0},
		{"service proxy", "t.me/proxy?server=1.2.3.4&port=1&secret=x", true, "unsupported", "", 0},
		{"service share", "t.me/share/url?url=https://x", true, "unsupported", "", 0},
		{"service addstickers", "t.me/addstickers/pack", true, "unsupported", "", 0},
		{"c without id", "t.me/c", true, "unsupported", "", 0},
		{"c non-numeric id", "t.me/c/abc", true, "unsupported", "", 0},
		{"short username", "t.me/ab", true, "unsupported", "", 0},
		{"username with dash", "t.me/not-a-channel", true, "unsupported", "", 0},
		{"bare host", "t.me", false, "", "", 0},
		{"bare host slash", "https://t.me/", false, "", "", 0},
		{"other host", "https://example.com/feed.xml", false, "", "", 0},
		{"garbage", "не ссылка", false, "", "", 0},
		{"empty", "", false, "", "", 0},
		{"prefix lookalike", "https://t.me.evil.com/x", false, "", "", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseTmeLink(tt.raw)
			if ok != tt.wantOK {
				t.Fatalf("ParseTmeLink(%q) ok = %v, want %v", tt.raw, ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if got.Kind != tt.wantKind || got.Username != tt.wantUser || got.BotAPIID != tt.wantID {
				t.Fatalf("ParseTmeLink(%q) = %+v, want kind=%q user=%q id=%d",
					tt.raw, got, tt.wantKind, tt.wantUser, tt.wantID)
			}
		})
	}
}
