package telegram

import (
	"sync"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
)

// maxBufferedItems bounds ChannelBuffer growth PER KEY. The buffer is shared
// by all managed channels the bot is admin of, including ones with no active
// source draining them (e.g. after /removesource) — without a cap, unread
// posts would accumulate for the life of the process. Before Stage 8 all keys
// additionally shared one slice, so the first draining ManagedSource stole
// the posts of every other channel; per-key storage fixes that.
const maxBufferedItems = 500

// ChannelBuffer is a thread-safe per-key buffer of feed.Items written by the
// bot's update handler and drained by ManagedSource.Collect. The key is the
// channel username, or the string form of the chat id for private chats where
// the bot is admin (no username).
type ChannelBuffer struct {
	mu    sync.Mutex
	items map[string][]feed.Item // lazily initialized; zero value is usable
}

// Push adds an item under the given key (called from the update handler
// goroutine). Oldest items of that key are dropped once it exceeds
// maxBufferedItems; other keys are untouched.
func (b *ChannelBuffer) Push(key string, item feed.Item) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.items == nil {
		b.items = make(map[string][]feed.Item)
	}
	b.items[key] = append(b.items[key], item)
	if n := len(b.items[key]); n > maxBufferedItems {
		b.items[key] = b.items[key][n-maxBufferedItems:]
	}
}

// Drain returns all items accumulated under the given key and clears only
// that key.
func (b *ChannelBuffer) Drain(key string) []feed.Item {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.items[key]
	delete(b.items, key)
	return out
}
