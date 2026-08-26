package app

import (
	"net/http"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
	"github.com/akomyagin/aiTelegaBot/internal/storage"
	"github.com/akomyagin/aiTelegaBot/internal/telegram"
)

// buildDynamicSources maps enabled DB rows to feed.Source implementations,
// reusing the same constructors as the static config layer. Disabled rows are
// skipped. Unknown kinds are skipped, never fatal.
//
// Known limitation: every tg_botapi source (static and dynamic) shares one
// process-wide channelBuf, and ManagedSource.Collect drains the WHOLE buffer
// regardless of which channel posted. With more than one active tg_botapi
// source, whichever one runs first in a given feed.Collect gets all buffered
// posts and the rest get none that run. Fixing this needs per-channel
// buffering (e.g. keyed by channel username), which is out of scope here.
func buildDynamicSources(
	dbSources []storage.Source,
	hc *http.Client,
	tgLimit int,
	channelBuf *telegram.ChannelBuffer, // always non-nil (app.go creates it unconditionally); nil-checked defensively
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
				out = append(out, telegram.NewManagedSource("@"+ds.Ref, channelBuf))
			}
		}
	}
	return out
}
