package mtproto

import "strings"

// firstLine extracts the first non-empty line (up to 100 runes) as a title.
// Duplicated from internal/telegram to avoid a cross-package dependency.
func firstLine(text string) string {
	text = strings.TrimSpace(text)
	if idx := strings.IndexByte(text, '\n'); idx >= 0 {
		text = text[:idx]
	}
	text = strings.TrimSpace(text)
	r := []rune(text)
	if len(r) > 100 {
		r = r[:100]
	}
	return string(r)
}

// botAPIChannelOffset is the Bot API marking of channels and supergroups: a
// peer with MTProto id X appears in the Bot API as -(1e12 + X) — the familiar
// "-100X" form (the Bot API literally prepends "-100" to the id digits).
const botAPIChannelOffset int64 = 1_000_000_000_000

// ToMTProtoID converts a Bot API chat id (-100X) into the MTProto peer id (X).
// ok=false for ids outside the channel/supergroup range (users, basic groups,
// overflow): those have no MTProto channel id, and their access_hash semantics
// differ. Valid inputs are -(1e12 + X) with 0 < X < 1e12.
func ToMTProtoID(botAPIID int64) (int64, bool) {
	if botAPIID <= -2*botAPIChannelOffset || botAPIID >= -botAPIChannelOffset {
		return 0, false
	}
	return -botAPIID - botAPIChannelOffset, true
}

// ToBotAPIID converts an MTProto channel/supergroup peer id (X) into its Bot
// API form (-100X). Callers pass X > 0.
func ToBotAPIID(mtprotoID int64) int64 {
	return -mtprotoID - botAPIChannelOffset
}
