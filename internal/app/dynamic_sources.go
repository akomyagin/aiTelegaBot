package app

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
	"github.com/akomyagin/aiTelegaBot/internal/mtproto"
	"github.com/akomyagin/aiTelegaBot/internal/storage"
	"github.com/akomyagin/aiTelegaBot/internal/telegram"
)

// buildDynamicSources maps enabled DB rows to feed.Source implementations,
// reusing the same constructors as the static config layer. Disabled rows are
// skipped. Unknown kinds are skipped, never fatal.
//
// Stage 8 A1 closed the "one shared channelBuf" tech debt: every tg_botapi row
// gets a ManagedSource bound to its OWN buffer key (the username, or the
// numeric "-100X" ref for private chats where the bot is admin). tg_mtproto
// rows resolve to a PeerSource (numeric "-100X" ref) or a ChannelSource
// (@username ref); they are skipped with a warning when MTProto is not
// configured (mtClient == nil — a first-class state on a VPS without
// MTPROTO_APP_ID/HASH).
func buildDynamicSources(
	dbSources []storage.Source,
	hc *http.Client,
	tgLimit int,
	channelBuf *telegram.ChannelBuffer, // always non-nil (app.go creates it unconditionally); nil-checked defensively
	mtClient *mtproto.Client, // nil when MTProto is not configured
	mtLimit int,
) []feed.Source {
	var out []feed.Source
	for _, ds := range dbSources {
		if !ds.Enabled {
			continue
		}
		switch ds.Kind {
		case "rss":
			out = append(out, feed.NewRSSSource(ds.Ref, ds.Ref, "rss", hc))
		case "tg_public":
			out = append(out, telegram.NewPublicSource("@"+ds.Ref, ds.Ref, hc, tgLimit))
		case "tg_botapi":
			if channelBuf != nil {
				name, key := managedSourceNameKey(ds.Ref)
				out = append(out, telegram.NewManagedSource(name, key, channelBuf))
			}
		case "tg_mtproto":
			if mtClient == nil {
				slog.Warn("skipping tg_mtproto source: MTProto is not configured", "ref", ds.Ref)
				continue
			}
			if botAPIID, err := strconv.ParseInt(ds.Ref, 10, 64); err == nil {
				if x, ok := mtproto.ToMTProtoID(botAPIID); ok {
					out = append(out, mtproto.NewPeerSource(ds.Ref, x, mtLimit, mtClient))
					continue
				}
			}
			ref := strings.TrimPrefix(ds.Ref, "@")
			out = append(out, mtproto.NewChannelSource("@"+ref, ref, mtLimit, mtClient))
		}
	}
	return out
}

// managedSourceNameKey derives the display name and the per-key buffer key
// from a tg_botapi ref: usernames buffer under themselves; numeric
// private-chat refs ("-100X", set when the bot admins a private chat) buffer
// under the numeric string — exactly the key handleUpdate picks (username,
// else chat id, plan §4.5).
func managedSourceNameKey(ref string) (name, key string) {
	if strings.HasPrefix(ref, "-") {
		return ref, ref
	}
	return "@" + ref, ref
}
