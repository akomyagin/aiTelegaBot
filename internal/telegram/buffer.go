package telegram

import (
	"sync"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
)

// maxBufferedItems bounds ChannelBuffer growth. The buffer is shared by all
// managed channels the bot is admin of, even ones with no active source
// draining them (e.g. after /removesource, or before any tg_botapi source is
// added) — without a cap, unread posts would accumulate for the life of the
// process.
const maxBufferedItems = 500

// ChannelBuffer is a thread-safe buffer of feed.Items written by the bot's
// update handler and drained by ManagedSource.Collect.
type ChannelBuffer struct {
	mu    sync.Mutex
	items []feed.Item
}

// Push adds an item (called from the update handler goroutine). Oldest items
// are dropped once the buffer exceeds maxBufferedItems.
func (b *ChannelBuffer) Push(item feed.Item) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.items = append(b.items, item)
	if len(b.items) > maxBufferedItems {
		b.items = b.items[len(b.items)-maxBufferedItems:]
	}
}

// Drain returns all accumulated items and clears the buffer.
func (b *ChannelBuffer) Drain() []feed.Item {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.items
	b.items = nil
	return out
}
