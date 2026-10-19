package mtproto

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/tg"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
)

// ChannelSource reads the history of a private (or any accessible) Telegram
// channel via MTProto (messages.getHistory) and returns the last N messages as
// []feed.Item. It implements feed.Source.
type ChannelSource struct {
	name    string  // human-readable name (@channel or display name)
	channel string  // username or "-100XXXXXXXXXX" (peer ID)
	limit   int     // max posts per collection
	client  *Client // MTProto client (not owned; outlives this source)
	log     *slog.Logger
}

// NewChannelSource builds a ChannelSource. The client is shared and must not be
// closed by the source.
func NewChannelSource(name, channel string, limit int, client *Client) *ChannelSource {
	return &ChannelSource{
		name:    name,
		channel: channel,
		limit:   limit,
		client:  client,
		log:     slog.Default(),
	}
}

// Name returns the human-readable source name.
func (s *ChannelSource) Name() string { return s.name }

// Collect returns the last s.limit messages from the channel. FloodWait backoff
// is handled inside Client.GetHistory; here we only normalize the result.
func (s *ChannelSource) Collect(ctx context.Context) ([]feed.Item, error) {
	msgs, err := s.client.GetHistory(ctx, s.channel, s.limit)
	if err != nil {
		return nil, err
	}
	items := make([]feed.Item, 0, len(msgs))
	for _, msg := range msgs {
		items = append(items, messageToFeedItem(msg, s.channel, channelDeepLink(s.channel, msg.ID)))
	}
	return items, nil
}

// messageToFeedItem normalizes a tg.Message into a feed.Item. source is the
// source ref (item.Source / DedupKey); url is the READY message deep link —
// callers know their peer best (ChannelSource: channelDeepLink by ref;
// PeerSource: the cached username decides public vs t.me/c form), so the link
// is built exactly once (review L8).
func messageToFeedItem(msg tg.Message, source, url string) feed.Item {
	return feed.Item{
		Kind:      "tg_mtproto",
		Source:    source,
		ID:        strconv.Itoa(msg.ID),
		Title:     firstLine(msg.Message),
		URL:       url,
		Text:      msg.Message,
		Published: time.Unix(int64(msg.Date), 0).UTC(),
	}
}

// channelDeepLink builds the t.me URL for one message given the source ref.
// Unparseable refs yield "" (no link is better than a wrong one).
func channelDeepLink(channel string, msgID int) string {
	if strings.HasPrefix(channel, "-") {
		botAPIID, err := strconv.ParseInt(channel, 10, 64)
		if err != nil {
			return ""
		}
		x, ok := ToMTProtoID(botAPIID)
		if !ok {
			return ""
		}
		return deepLinkURL("", x, msgID)
	}
	return deepLinkURL(strings.TrimPrefix(channel, "@"), 0, msgID)
}
