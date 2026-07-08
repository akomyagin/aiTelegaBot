package telegram

import (
	"context"
	"fmt"
	"time"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
)

// ManagedSource reads Telegram channel posts from channels/groups where the bot
// is an admin. Posts are buffered by handleUpdate and drained on Collect.
type ManagedSource struct {
	name string
	buf  *ChannelBuffer
}

// NewManagedSource builds a source draining posts of managed channels.
func NewManagedSource(name string, buf *ChannelBuffer) *ManagedSource {
	return &ManagedSource{name: name, buf: buf}
}

// Name returns the human-readable source name.
func (s *ManagedSource) Name() string { return s.name }

// Collect drains all posts buffered since the previous call.
func (s *ManagedSource) Collect(_ context.Context) ([]feed.Item, error) {
	return s.buf.Drain(), nil
}

// channelPostToItem normalises a Telegram channel post to feed.Item.
func channelPostToItem(chatUsername string, msgID int, text string, date int64) feed.Item {
	title := firstLine(text)
	url := ""
	if chatUsername != "" {
		url = fmt.Sprintf("https://t.me/%s/%d", chatUsername, msgID)
	}
	return feed.Item{
		Kind:      "tg_botapi",
		Source:    chatUsername,
		ID:        fmt.Sprintf("%d", msgID),
		Title:     title,
		URL:       url,
		Text:      text,
		Published: time.Unix(date, 0).UTC(),
	}
}
