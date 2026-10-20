package app

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/akomyagin/aiTelegaBot/internal/digest"
	"github.com/akomyagin/aiTelegaBot/internal/feed"
	"github.com/akomyagin/aiTelegaBot/internal/mtproto"
	"github.com/akomyagin/aiTelegaBot/internal/storage"
	"github.com/akomyagin/aiTelegaBot/internal/telegram"
)

// sourceLister is the storage slice newSourceProvider needs — an interface so
// tests can count ListSources calls with a fake (план C, тест-кейс 7: одна
// выборка на прогон). *storage.SQLiteStore satisfies it.
type sourceLister interface {
	ListSources(ctx context.Context) ([]storage.Source, error)
}

// buildDynamicSources maps enabled DB rows to tracked feed sources (Stage 8 C:
// digest.TrackedSource — SourceID/TopicID come from the row), reusing the same
// constructors as the static config layer. Disabled rows are skipped. Unknown
// kinds are skipped, never fatal.
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
) []digest.TrackedSource {
	var out []digest.TrackedSource
	for _, ds := range dbSources {
		if !ds.Enabled {
			continue
		}
		// Строки origin='static' (подэтап B) — те же источники, что уже
		// построены статическим слоем из config: без фильтра здесь были бы
		// дубли (план 614–615).
		if ds.Origin == "static" {
			continue
		}
		var src feed.Source
		switch ds.Kind {
		case "rss":
			src = feed.NewRSSSource(ds.Ref, ds.Ref, "rss", hc)
		case "tg_public":
			src = telegram.NewPublicSource("@"+ds.Ref, ds.Ref, hc, tgLimit)
		case "tg_botapi":
			if channelBuf == nil {
				continue
			}
			name, key := managedSourceNameKey(ds.Ref)
			src = telegram.NewManagedSource(name, key, channelBuf)
		case "tg_mtproto":
			if mtClient == nil {
				slog.Warn("skipping tg_mtproto source: MTProto is not configured", "ref", ds.Ref)
				continue
			}
			if botAPIID, err := strconv.ParseInt(ds.Ref, 10, 64); err == nil {
				if x, ok := mtproto.ToMTProtoID(botAPIID); ok {
					src = mtproto.NewPeerSource(ds.Ref, x, mtLimit, mtClient)
					break
				}
			}
			ref := strings.TrimPrefix(ds.Ref, "@")
			src = mtproto.NewChannelSource("@"+ref, ref, mtLimit, mtClient)
		default:
			continue
		}
		out = append(out, digest.TrackedSource{
			Src: src, SourceID: ds.ID, TopicID: ds.TopicID,
		})
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

// newSourceProvider builds the Pipeline.SourceProvider closure (Stage 8 C,
// план §C): ОДНА выборка ListSources на прогон; статики из config получают
// SourceID/TopicID по совпадению (kind, ref) своей строки origin='static'
// (строки нет — например upsert не сработал или pruning-гонка — → нули →
// «Без темы», прогон не останавливается; enabled у static-строк игнорируется,
// как и раньше — §4.1: состав решает config); динамики — из своих строк
// (buildDynamicSources). sourceLister — seam для теста со счётчиком вызовов.
func newSourceProvider(
	sl sourceLister,
	staticSources []feed.Source,
	staticRefs []staticSourceRef,
	hc *http.Client,
	tgLimit int,
	channelBuf *telegram.ChannelBuffer,
	mtClient *mtproto.Client,
	mtLimit int,
) func(ctx context.Context) ([]digest.TrackedSource, error) {
	return func(ctx context.Context) ([]digest.TrackedSource, error) {
		dbRows, err := sl.ListSources(ctx)
		if err != nil {
			return nil, err
		}
		byRef := make(map[storage.StaticRef]storage.Source, len(dbRows))
		for _, row := range dbRows {
			byRef[storage.StaticRef{Kind: row.Kind, Ref: row.Ref}] = row
		}
		// Static first, then dynamic; order is cosmetic (dedup handles overlap).
		all := make([]digest.TrackedSource, 0, len(staticSources)+len(dbRows))
		for i, src := range staticSources {
			ts := digest.TrackedSource{Src: src}
			if row, ok := byRef[storage.StaticRef{Kind: staticRefs[i].Kind, Ref: staticRefs[i].Ref}]; ok {
				ts.SourceID = row.ID
				ts.TopicID = row.TopicID
			}
			all = append(all, ts)
		}
		all = append(all, buildDynamicSources(dbRows, hc, tgLimit, channelBuf, mtClient, mtLimit)...)
		return all, nil
	}
}
