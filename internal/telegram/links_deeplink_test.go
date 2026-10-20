// DeepLink (Stage 8 D-1, план §D) — обратная конверсия к ParseTmeLink:
// публичный/приватный peer, -100 strip без импорта mtproto, мусор → "".

package telegram

import "testing"

func TestDeepLink(t *testing.T) {
	tests := []struct {
		name   string
		tgPeer string
		msgID  int64
		want   string
	}{
		{"публичный username", "durov", 123, "https://t.me/durov/123"},
		{"username с @", "@durov", 7, "https://t.me/durov/7"},
		{"приватный -100X", "-1001234567890", 55, "https://t.me/c/1234567890/55"},
		{"пустой peer", "", 5, ""},
		{"peer из пробелов", "   ", 5, ""},
		{"msgID 0", "durov", 0, ""},
		{"msgID отрицательный", "durov", -3, ""},
		{"нечисловой остаток после -100", "-100abc", 5, ""},
		{"голый -100", "-100", 5, ""},
		{"ноль после -100", "-1000", 5, ""},
		{"X >= 1e12 (граница mtproto.ToMTProtoID)", "-1001000000000000", 5, ""},
		{"overflow int64", "-10099999999999999999999", 5, ""},
		{"мусорный peer", "не peer", 5, ""},
		{"слишком короткий username", "ab", 5, ""},
		{"отрицательный не-канал", "-123", 5, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DeepLink(tt.tgPeer, tt.msgID); got != tt.want {
				t.Errorf("DeepLink(%q, %d) = %q, want %q", tt.tgPeer, tt.msgID, got, tt.want)
			}
		})
	}

	// Round-trip с ParseTmeLink: deep link приватного peer парсится обратно в
	// тот же BotAPIID, публичного — в тот же username.
	parsed, ok := ParseTmeLink(DeepLink("-1001234567890", 5))
	if !ok || parsed.Kind != "private" || parsed.BotAPIID != -1001234567890 {
		t.Errorf("round-trip private = %+v ok=%v, want {-1001234567890}", parsed, ok)
	}
	parsedPub, okPub := ParseTmeLink(DeepLink("@durov", 5))
	if !okPub || parsedPub.Kind != "public" || parsedPub.Username != "durov" {
		t.Errorf("round-trip public = %+v ok=%v, want {durov}", parsedPub, okPub)
	}
}
