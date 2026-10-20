package telegram

import (
	"strconv"
	"strings"
)

// tmeChannelOffset is the Bot API marking of channels/supergroups: a peer with
// MTProto id X appears in the Bot API as -(1e12 + X) — the familiar "-100X"
// form. Mirrors mtproto.ToBotAPIID; kept local so the telegram package does
// not depend on mtproto.
const tmeChannelOffset int64 = 1_000_000_000_000

// TmeLink is a parsed t.me / telegram.me link (Stage 8 A1).
type TmeLink struct {
	Kind     string // "public" | "private" | "unsupported"
	Username string // Kind=="public": channel username WITHOUT '@'
	BotAPIID int64  // Kind=="private": -100X form of t.me/c/<X>
}

// tmeHosts are the hosts treated as Telegram links.
var tmeHosts = map[string]bool{"t.me": true, "telegram.me": true}

// tmeServicePrefixes are reserved t.me path prefixes that never denote a
// channel (invites, sharing, proxies, sticker packs, …). Registering such a
// link is out of scope; the bot answers with an explicit refusal.
var tmeServicePrefixes = map[string]bool{
	"joinchat":     true,
	"proxy":        true,
	"socks":        true,
	"share":        true,
	"addstickers":  true,
	"addtheme":     true,
	"addlist":      true,
	"addemoji":     true,
	"stickers":     true,
	"iv":           true,
	"bg":           true,
	"confirmphone": true,
	"donate":       true,
	"boost":        true,
	"giftcode":     true,
	"invoice":      true,
}

// ParseTmeLink parses a t.me / telegram.me link. The returned bool is false
// when raw is not a t.me link at all (the caller then falls back to other
// kinds, e.g. RSS). Accepted forms:
//
//	t.me/<user>, telegram.me/<user>, https://t.me/<user>  -> public
//	t.me/s/<user> (web-preview prefix stripped)           -> public
//	t.me/c/<X>[/<msg>]                                    -> private, X -> -100X
//
// Invite links (t.me/+…, /joinchat) and other service links are reported as
// Kind "unsupported" so the caller can refuse them explicitly. Extra path
// segments (message links t.me/<user>/<msg>) are accepted: the first segment
// is the username.
func ParseTmeLink(raw string) (TmeLink, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return TmeLink{}, false
	}
	// Strip the (optional) scheme.
	low := strings.ToLower(s)
	for _, prefix := range []string{"https://", "http://"} {
		if strings.HasPrefix(low, prefix) {
			s = s[len(prefix):]
			low = low[len(prefix):]
			break
		}
	}
	// Drop query/fragment.
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}

	host := s
	path := ""
	if i := strings.IndexByte(s, '/'); i >= 0 {
		host, path = s[:i], s[i+1:]
	}
	if !tmeHosts[strings.ToLower(host)] {
		return TmeLink{}, false
	}
	path = strings.Trim(path, "/")
	if path == "" {
		return TmeLink{}, false // bare t.me — nothing to register
	}

	segments := strings.Split(path, "/")
	first := segments[0]
	rest := segments[1:]

	switch {
	case strings.HasPrefix(first, "+"):
		return TmeLink{Kind: "unsupported"}, true // invite link t.me/+hash
	case first == "s" && len(rest) > 0:
		first = rest[0] // t.me/s/<user> — public web preview; fall through below
	case first == "c":
		if len(rest) == 0 {
			return TmeLink{Kind: "unsupported"}, true
		}
		x, err := strconv.ParseInt(rest[0], 10, 64)
		// The upper bound keeps -tmeChannelOffset-x inside int64 and mirrors
		// mtproto.ToMTProtoID, which rejects X >= 1e12 (review L3).
		if err != nil || x <= 0 || x >= tmeChannelOffset {
			return TmeLink{Kind: "unsupported"}, true
		}
		return TmeLink{Kind: "private", BotAPIID: -tmeChannelOffset - x}, true
	case tmeServicePrefixes[strings.ToLower(first)]:
		return TmeLink{Kind: "unsupported"}, true
	}

	if !validTmeUsername(first) {
		return TmeLink{Kind: "unsupported"}, true
	}
	return TmeLink{Kind: "public", Username: first}, true
}

// validTmeUsername reports whether s looks like a Telegram username
// (5–32 chars of [A-Za-z0-9_]). Links to users parse as "public" too; the
// trial collect downstream surfaces "not a channel" for them.
func validTmeUsername(s string) bool {
	if len(s) < 5 || len(s) > 32 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
		default:
			return false
		}
	}
	return true
}

// DeepLink builds the t.me deep link of a message from its stored tg_peer
// (Stage 8 D, план §D) — the inverse of ParseTmeLink, kept symmetric to it.
// tgPeer is the digest_items.tg_peer form: a username (with or without '@')
// → public link https://t.me/<user>/<msgID>; the Bot-API "-100X" form →
// private link https://t.me/c/<X>/<msgID> (the -100 prefix is stripped
// locally, mirroring mtproto.ToMTProtoID WITHOUT importing mtproto).
// Garbage yields "": empty peer, msgID <= 0, a non-numeric/zero/oversized
// remainder after the -100 strip, or a peer that is not a valid username
// (the same validTmeUsername check ParseTmeLink applies).
func DeepLink(tgPeer string, msgID int64) string {
	if msgID <= 0 {
		return ""
	}
	peer := strings.TrimSpace(tgPeer)
	if peer == "" {
		return ""
	}
	if rest, ok := strings.CutPrefix(peer, "-100"); ok {
		x, err := strconv.ParseInt(rest, 10, 64)
		// The upper bound mirrors ParseTmeLink/mtproto.ToMTProtoID: X >= 1e12
		// is not a valid channel id.
		if err != nil || x <= 0 || x >= tmeChannelOffset {
			return ""
		}
		return "https://t.me/c/" + strconv.FormatInt(x, 10) + "/" + strconv.FormatInt(msgID, 10)
	}
	user := strings.TrimPrefix(peer, "@")
	if !validTmeUsername(user) {
		return ""
	}
	return "https://t.me/" + user + "/" + strconv.FormatInt(msgID, 10)
}
