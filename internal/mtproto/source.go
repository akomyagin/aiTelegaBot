package mtproto

import (
	"context"
	"log/slog"
	"strconv"
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
		items = append(items, messageToFeedItem(msg, s.channel))
	}
	return items, nil
}

// messageToFeedItem normalizes a tg.Message into a feed.Item. Private channels
// have no public URL, so URL is left empty.
func messageToFeedItem(msg tg.Message, channel string) feed.Item {
	return feed.Item{
		Kind:      "tg_mtproto",
		Source:    channel,
		ID:        strconv.Itoa(msg.ID),
		Title:     firstLine(msg.Message),
		URL:       "",
		Text:      msg.Message,
		Published: time.Unix(int64(msg.Date), 0).UTC(),
	}
}
