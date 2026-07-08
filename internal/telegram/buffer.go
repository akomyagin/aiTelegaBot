package telegram

import (
	"sync"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
)

// ChannelBuffer is a thread-safe buffer of feed.Items written by the bot's
// update handler and drained by ManagedSource.Collect.
type ChannelBuffer struct {
	mu    sync.Mutex
	items []feed.Item
}

// Push adds an item (called from the update handler goroutine).
func (b *ChannelBuffer) Push(item feed.Item) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.items = append(b.items, item)
}

// Drain returns all accumulated items and clears the buffer.
func (b *ChannelBuffer) Drain() []feed.Item {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.items
	b.items = nil
	return out
}
