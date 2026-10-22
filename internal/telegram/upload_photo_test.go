// Тесты UploadPhoto (подэтап 9E-2b, план §4.5.1 п.2) через test-seam
// uploadPhotoFn — паттерн photo_test.go (SendPhoto/DeleteMessage, 9E-1).

package telegram

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-telegram/bot/models"
)

type uploadRec struct {
	chatIDs []int64
	photos  []models.InputFile
	id      int64
	sizes   []models.PhotoSize
	err     error
}

func (r *uploadRec) fn(_ context.Context, chatID int64, photo models.InputFile) (int64, []models.PhotoSize, error) {
	r.chatIDs = append(r.chatIDs, chatID)
	r.photos = append(r.photos, photo)
	return r.id, r.sizes, r.err
}

func tempPhotoFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "photo-42.jpg")
	if err := os.WriteFile(p, []byte("photo-bytes"), 0o600); err != nil {
		t.Fatalf("write temp photo: %v", err)
	}
	return p
}

func TestUploadPhoto(t *testing.T) {
	twoSizes := []models.PhotoSize{{FileID: "small"}, {FileID: "large"}}

	t.Run("seam: owner-чат, largest fileID, multipart-форма", func(t *testing.T) {
		rec := &uploadRec{id: 7, sizes: twoSizes}
		b := &Bot{chatID: 111, log: discardLogger(), uploadPhotoFn: rec.fn}
		id, fileID, err := b.UploadPhoto(context.Background(), "", tempPhotoFile(t), "photo-42.jpg")
		if err != nil {
			t.Fatalf("UploadPhoto: %v", err)
		}
		if id != 7 || fileID != "large" {
			t.Errorf("id=%d fileID=%q, want 7/large", id, fileID)
		}
		if len(rec.chatIDs) != 1 || rec.chatIDs[0] != 111 {
			t.Errorf("chatIDs = %v, want [111] (owner)", rec.chatIDs)
		}
		up, ok := rec.photos[0].(*models.InputFileUpload)
		if !ok {
			t.Fatalf("photo type = %T, want *models.InputFileUpload", rec.photos[0])
		}
		if up.Filename != "photo-42.jpg" || up.Data == nil {
			t.Errorf("upload = {Filename:%q Data:%v}", up.Filename, up.Data)
		}
	})

	t.Run("fileName пуст → базовое имя файла", func(t *testing.T) {
		rec := &uploadRec{id: 1, sizes: twoSizes}
		b := &Bot{chatID: 111, log: discardLogger(), uploadPhotoFn: rec.fn}
		path := tempPhotoFile(t)
		if _, _, err := b.UploadPhoto(context.Background(), "", path, ""); err != nil {
			t.Fatalf("UploadPhoto: %v", err)
		}
		up := rec.photos[0].(*models.InputFileUpload)
		if up.Filename != filepath.Base(path) {
			t.Errorf("Filename = %q, want %q", up.Filename, filepath.Base(path))
		}
	})

	t.Run("явный chatID", func(t *testing.T) {
		rec := &uploadRec{id: 1, sizes: twoSizes}
		b := &Bot{chatID: 111, log: discardLogger(), uploadPhotoFn: rec.fn}
		if _, _, err := b.UploadPhoto(context.Background(), "222", tempPhotoFile(t), "p.jpg"); err != nil {
			t.Fatalf("UploadPhoto: %v", err)
		}
		if rec.chatIDs[0] != 222 {
			t.Errorf("chatID = %d, want 222", rec.chatIDs[0])
		}
	})

	t.Run("ошибка seam → wrapped error", func(t *testing.T) {
		rec := &uploadRec{err: errors.New("send boom")}
		b := &Bot{chatID: 111, log: discardLogger(), uploadPhotoFn: rec.fn}
		_, _, err := b.UploadPhoto(context.Background(), "", tempPhotoFile(t), "p.jpg")
		if err == nil || !strings.Contains(err.Error(), "telegram upload photo") {
			t.Fatalf("err = %v, want wrapped 'telegram upload photo'", err)
		}
	})

	t.Run("пустое Photo в ответе → error (file_id не получить)", func(t *testing.T) {
		rec := &uploadRec{id: 9} // sizes nil
		b := &Bot{chatID: 111, log: discardLogger(), uploadPhotoFn: rec.fn}
		_, _, err := b.UploadPhoto(context.Background(), "", tempPhotoFile(t), "p.jpg")
		if err == nil || !strings.Contains(err.Error(), "no photo") {
			t.Fatalf("err = %v, want 'no photo'", err)
		}
	})

	t.Run("без seam и без api → error, не silent no-op", func(t *testing.T) {
		b := &Bot{chatID: 111, log: discardLogger()}
		_, _, err := b.UploadPhoto(context.Background(), "", tempPhotoFile(t), "p.jpg")
		if err == nil || !strings.Contains(err.Error(), "no transport") {
			t.Fatalf("err = %v, want 'no transport'", err)
		}
	})

	t.Run("пустой filePath → error", func(t *testing.T) {
		b := &Bot{chatID: 111, log: discardLogger(), uploadPhotoFn: (&uploadRec{}).fn}
		if _, _, err := b.UploadPhoto(context.Background(), "", "  ", "p.jpg"); err == nil {
			t.Fatal("want error for empty file path")
		}
	})

	t.Run("несуществующий файл → error", func(t *testing.T) {
		b := &Bot{chatID: 111, log: discardLogger(), uploadPhotoFn: (&uploadRec{}).fn}
		_, _, err := b.UploadPhoto(context.Background(), "", filepath.Join(t.TempDir(), "nope.jpg"), "p.jpg")
		if err == nil {
			t.Fatal("want error for missing file")
		}
	})

	t.Run("мусорный chatID → error", func(t *testing.T) {
		b := &Bot{chatID: 111, log: discardLogger(), uploadPhotoFn: (&uploadRec{}).fn}
		_, _, err := b.UploadPhoto(context.Background(), "abc", tempPhotoFile(t), "p.jpg")
		if err == nil || !strings.Contains(err.Error(), "invalid chat id") {
			t.Fatalf("err = %v, want 'invalid chat id'", err)
		}
	})
}
