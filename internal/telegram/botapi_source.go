package telegram

import (
	"context"
	"fmt"
	"time"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
)

// ManagedSource reads Telegram channel posts from ONE channel/group where the
// bot is an admin. Posts are buffered by handleUpdate under key and drained on
// Collect; other channels' posts (other keys) are left alone.
type ManagedSource struct {
	name string
	key  string // buffer key: username, or numeric chat id for private chats
	buf  *ChannelBuffer
}

// NewManagedSource builds a source draining posts of one managed channel
// identified by its buffer key (username, or the chat id as a string).
func NewManagedSource(name, key string, buf *ChannelBuffer) *ManagedSource {
	return &ManagedSource{name: name, key: key, buf: buf}
}

// Name returns the human-readable source name.
func (s *ManagedSource) Name() string { return s.name }

// Collect drains the posts buffered for this source's key since the previous
// call.
func (s *ManagedSource) Collect(_ context.Context) ([]feed.Item, error) {
	return s.buf.Drain(s.key), nil
}

// channelPostToItem normalises a Telegram channel post to feed.Item. source is
// the dedup/identity key (username, or numeric chat id for private chats);
// chatUsername is empty for private chats, where no public t.me URL exists.
func channelPostToItem(source, chatUsername string, msgID int, text string, date int64) feed.Item {
	title := firstLine(text)
	url := ""
	if chatUsername != "" {
		url = fmt.Sprintf("https://t.me/%s/%d", chatUsername, msgID)
	}
	return feed.Item{
		Kind:      "tg_botapi",
		Source:    source,
		ID:        fmt.Sprintf("%d", msgID),
		Title:     title,
		URL:       url,
		Text:      text,
		Published: time.Unix(date, 0).UTC(),
	}
}
