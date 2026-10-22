package telegram

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/go-telegram/bot/models"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
)

func TestChannelPostToItem(t *testing.T) {
	item := channelPostToItem("mychan", "mychan", 42, "Hello world\nsecond", 1705309200)

	if item.Kind != "tg_botapi" {
		t.Errorf("Kind = %q, want tg_botapi", item.Kind)
	}
	if item.URL != "https://t.me/mychan/42" {
		t.Errorf("URL = %q", item.URL)
	}
	if item.Title != "Hello world" {
		t.Errorf("Title = %q, want %q", item.Title, "Hello world")
	}
	if got := item.DedupKey(); got != "tg:mychan:42" {
		t.Errorf("DedupKey() = %q, want tg:mychan:42", got)
	}
}

// TestChannelPostToItem_PrivateChat covers the key/username split (Stage 8):
// a private chat post has no username, so Source is the numeric key and no
// public t.me URL exists.
func TestChannelPostToItem_PrivateChat(t *testing.T) {
	item := channelPostToItem("-1001234567890", "", 7, "Private post", 1705309200)

	if item.Source != "-1001234567890" {
		t.Errorf("Source = %q, want numeric key", item.Source)
	}
	if item.URL != "" {
		t.Errorf("URL = %q, want empty for private chat", item.URL)
	}
	if got := item.DedupKey(); got != "tg:-1001234567890:7" {
		t.Errorf("DedupKey() = %q", got)
	}
}

// TestLargestPhotoFileID (Stage 9E, тест-кейс №1): Bot API сортирует Photo по
// возрастанию размера — largest это последний элемент; пустой список → "".
func TestLargestPhotoFileID(t *testing.T) {
	if got := largestPhotoFileID(nil); got != "" {
		t.Errorf("nil Photo = %q, want empty", got)
	}
	photo := []models.PhotoSize{
		{FileID: "small", Width: 90, Height: 90},
		{FileID: "large", Width: 800, Height: 600},
	}
	if got := largestPhotoFileID(photo); got != "large" {
		t.Errorf("largestPhotoFileID() = %q, want large", got)
	}
}

// TestHandleUpdate_ChannelPostPhoto (Stage 9E, тест-кейс №1): ChannelPost с
// Photo → MediaKind=photo + largest file_id; альбом (MediaGroupID) — фото несёт
// только ПЕРВЫЙ пост группы; пост без Photo → media-поля пустые.
func TestHandleUpdate_ChannelPostPhoto(t *testing.T) {
	buf := &ChannelBuffer{}
	b := &Bot{channelBuf: buf}
	ctx := context.Background()

	post := func(id int, photo []models.PhotoSize, groupID string) *models.Update {
		return &models.Update{ChannelPost: &models.Message{
			ID:           id,
			Chat:         models.Chat{Username: "mychan"},
			Text:         "post",
			Date:         1705309200,
			Photo:        photo,
			MediaGroupID: groupID,
		}}
	}
	twoSizes := []models.PhotoSize{{FileID: "small"}, {FileID: "large"}}

	b.handleUpdate(ctx, nil, post(1, twoSizes, ""))    // одиночное фото
	b.handleUpdate(ctx, nil, post(2, twoSizes, "grp")) // альбом, первый пост
	b.handleUpdate(ctx, nil, post(3, twoSizes, "grp")) // альбом, второй пост
	b.handleUpdate(ctx, nil, post(4, nil, ""))         // без фото

	items := buf.Drain("mychan")
	if len(items) != 4 {
		t.Fatalf("Drain() len = %d, want 4", len(items))
	}
	if items[0].MediaKind != "photo" || items[0].MediaFileID != "large" {
		t.Errorf("solo photo: MediaKind=%q MediaFileID=%q, want photo/large",
			items[0].MediaKind, items[0].MediaFileID)
	}
	if items[1].MediaKind != "photo" || items[1].MediaFileID != "large" {
		t.Errorf("album first: MediaKind=%q MediaFileID=%q, want photo/large",
			items[1].MediaKind, items[1].MediaFileID)
	}
	if items[2].MediaKind != "" || items[2].MediaFileID != "" {
		t.Errorf("album second: media-поля должны быть пустыми, got %q/%q",
			items[2].MediaKind, items[2].MediaFileID)
	}
	if items[3].MediaKind != "" || items[3].MediaFileID != "" {
		t.Errorf("no photo: media-поля должны быть пустыми, got %q/%q",
			items[3].MediaKind, items[3].MediaFileID)
	}
	// Текст/идентичность не пострадали от медиа-ветки.
	if items[0].DedupKey() != "tg:mychan:1" {
		t.Errorf("DedupKey() = %q, want tg:mychan:1", items[0].DedupKey())
	}
}

func TestChannelBuffer_Concurrent(t *testing.T) {
	buf := &ChannelBuffer{}
	var wg sync.WaitGroup
	const perGoroutine = 10
	const goroutines = 10 // total pushed (100) stays well under maxBufferedItems
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				buf.Push("c", feed.Item{Kind: "tg_botapi", Source: "c", ID: "x"})
			}
		}()
	}
	wg.Wait()

	got := buf.Drain("c")
	want := goroutines * perGoroutine
	if len(got) != want {
		t.Fatalf("Drain() len = %d, want %d", len(got), want)
	}
	if len(buf.Drain("c")) != 0 {
		t.Errorf("second Drain() should be empty")
	}
}

// TestChannelBuffer_CapsAtMaxBufferedItems confirms Push bounds memory growth:
// once the buffer exceeds maxBufferedItems, the oldest items are dropped so a
// channel with no active drainer cannot grow the buffer without limit.
func TestChannelBuffer_CapsAtMaxBufferedItems(t *testing.T) {
	buf := &ChannelBuffer{}
	total := maxBufferedItems + 50
	for i := 0; i < total; i++ {
		buf.Push("k", feed.Item{ID: fmt.Sprintf("%d", i)})
	}

	got := buf.Drain("k")
	if len(got) != maxBufferedItems {
		t.Fatalf("Drain() len = %d, want %d (capped)", len(got), maxBufferedItems)
	}
	// The dropped items should be the OLDEST ones — the newest item pushed
	// (total-1) must be the last one retained.
	if last := got[len(got)-1].ID; last != fmt.Sprintf("%d", total-1) {
		t.Errorf("newest item not retained: last item ID = %q, want %q", last, fmt.Sprintf("%d", total-1))
	}
}

func TestManagedSource_Collect(t *testing.T) {
	buf := &ChannelBuffer{}
	buf.Push("chan", feed.Item{Kind: "tg_botapi", ID: "1"})
	src := NewManagedSource("@chan", "chan", buf)

	items, err := src.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	if src.Name() != "@chan" {
		t.Errorf("Name() = %q", src.Name())
	}
}
