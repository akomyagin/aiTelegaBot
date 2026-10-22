package telegram

import (
	"os"
	"testing"
)

// TestParseChannelHTML_Photo (Stage 9E, тест-кейс №1): пост с
// tgme_widget_message_photo_wrap → MediaKind=photo + MediaURL из
// background-image; пост без фото → media-поля пустые.
func TestParseChannelHTML_Photo(t *testing.T) {
	f, err := os.Open("testdata/tme_channel_photo.html")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	items, err := parseChannelHTML(f, "testchan", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
	if items[0].MediaKind != "photo" {
		t.Errorf("photo post: MediaKind = %q, want photo", items[0].MediaKind)
	}
	if want := "https://cdn.telegram.org/file/abc.jpg"; items[0].MediaURL != want {
		t.Errorf("photo post: MediaURL = %q, want %q", items[0].MediaURL, want)
	}
	if items[0].MediaFileID != "" {
		t.Errorf("photo post: MediaFileID = %q, want empty (у tg_public нет file_id)", items[0].MediaFileID)
	}
	if items[1].MediaKind != "" || items[1].MediaURL != "" {
		t.Errorf("plain post: media-поля должны быть пустыми, got %q/%q",
			items[1].MediaKind, items[1].MediaURL)
	}
}
