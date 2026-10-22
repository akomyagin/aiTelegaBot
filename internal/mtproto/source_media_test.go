package mtproto

import (
	"testing"

	"github.com/gotd/td/tg"
)

// TestMessageToFeedItem_Media (Stage 9E, тест-кейс №1): MessageMediaPhoto →
// MediaKind="photo" без file_id/URL (их резолвит прогон, план §4.5.1 — часть
// 2b подэтапа 9E); video/document и отсутствие медиа → поля пустые. Фейк на
// уровне нормализации: реальный gotd-клиент не мокаем (существующая практика).
func TestMessageToFeedItem_Media(t *testing.T) {
	tests := []struct {
		name     string
		msg      tg.Message
		wantKind string
	}{
		{
			name:     "photo",
			msg:      tg.Message{ID: 1, Media: &tg.MessageMediaPhoto{Photo: &tg.Photo{ID: 123}}},
			wantKind: "photo",
		},
		{
			name:     "photo_empty",
			msg:      tg.Message{ID: 2, Media: &tg.MessageMediaPhoto{Photo: &tg.PhotoEmpty{}}},
			wantKind: "",
		},
		{
			name:     "video_document_ignored",
			msg:      tg.Message{ID: 3, Media: &tg.MessageMediaDocument{Document: &tg.Document{ID: 9}}},
			wantKind: "",
		},
		{
			name:     "no_media",
			msg:      tg.Message{ID: 4},
			wantKind: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := messageToFeedItem(tt.msg, "chan", "")
			if item.MediaKind != tt.wantKind {
				t.Errorf("MediaKind = %q, want %q", item.MediaKind, tt.wantKind)
			}
			// tg_mtproto никогда не несёт file_id/URL на этапе нормализации —
			// фото приватного канала резолвится download→re-upload в прогоне.
			if item.MediaFileID != "" || item.MediaURL != "" {
				t.Errorf("MediaFileID/MediaURL = %q/%q, want пустые",
					item.MediaFileID, item.MediaURL)
			}
		})
	}
}
