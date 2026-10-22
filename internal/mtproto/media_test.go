// Тесты MediaResolver (подэтап 9E, план тест-кейс №4): фейковые downloader +
// uploader, без сети. Реальный gotd-клиент (DownloadPhoto) не мокаем —
// существующая практика: клиентские методы mtproto тестируются на уровне
// нормализации; путь stream-записи покрыт кодом (стриминг chunk'ами).

package mtproto

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strconv"
	"testing"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
)

// --- фейки ---

type fakeDownloader struct {
	paths   []string     // destPath каждого вызова (проверка удаления temp-файлов)
	msgIDs  []int        // msgID каждого вызова
	failIDs map[int]bool // ошибка download по msgID
	err     error        // глобальная ошибка
}

func (f *fakeDownloader) DownloadPhoto(_ context.Context, _ string, msgID int, destPath string) error {
	f.paths = append(f.paths, destPath)
	f.msgIDs = append(f.msgIDs, msgID)
	if f.err != nil {
		return f.err
	}
	if f.failIDs[msgID] {
		return errors.New("download boom " + strconv.Itoa(msgID))
	}
	return os.WriteFile(destPath, []byte("photo-bytes"), 0o600)
}

type fakeUploader struct {
	uploads int
	deletes []int64
	fileErr error
	delErr  error
}

func (f *fakeUploader) UploadPhoto(_ context.Context, _, _, fileName string) (int64, string, error) {
	f.uploads++
	if f.fileErr != nil {
		return 0, "", f.fileErr
	}
	return int64(900 + f.uploads), "fid-" + fileName, nil
}

func (f *fakeUploader) DeleteMessage(_ context.Context, _ string, messageID int64) error {
	f.deletes = append(f.deletes, messageID)
	return f.delErr
}

func discardMediaLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func mtPhotoItem(msgID int) feed.Item {
	return feed.Item{
		Kind: "tg_mtproto", Source: "@privchan", ID: strconv.Itoa(msgID),
		Title: "T" + strconv.Itoa(msgID),
		URL:   "https://t.me/c/100/" + strconv.Itoa(msgID),
		Text:  "private text", MediaKind: "photo",
	}
}

// assertTempFilesRemoved проверяет, что все temp-файлы downloader'а удалены
// (крах/успех — мусора не остаётся, §4.5.1).
func assertTempFilesRemoved(t *testing.T, paths []string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("temp file %s still exists after Resolve", p)
		}
	}
}

// --- лимит (тест-кейс №4: лимит 3 → 4-й элемент без media_ref) ---

func TestMediaResolver_BudgetLimit(t *testing.T) {
	down := &fakeDownloader{}
	up := &fakeUploader{}
	r := NewMediaResolver(down, up, discardMediaLog())
	items := []feed.Item{mtPhotoItem(1), mtPhotoItem(2), mtPhotoItem(3), mtPhotoItem(4)}

	used, err := r.Resolve(context.Background(), items, 3)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if used != 3 {
		t.Errorf("used = %d, want 3", used)
	}
	for i := 0; i < 3; i++ {
		want := "fid-photo-" + items[i].ID + ".jpg"
		if items[i].MediaFileID != want {
			t.Errorf("items[%d].MediaFileID = %q, want %q", i, items[i].MediaFileID, want)
		}
	}
	if items[3].MediaFileID != "" {
		t.Errorf("4-й элемент получил media сверх бюджета: %q", items[3].MediaFileID)
	}
	if len(down.msgIDs) != 3 {
		t.Errorf("download calls = %d, want 3 (budget)", len(down.msgIDs))
	}
	if up.uploads != 3 {
		t.Errorf("uploads = %d, want 3", up.uploads)
	}
	// DeleteMessage вызван после КАЖДОГО успешного re-upload — служебные
	// сообщения не остаются (тест-кейс №4).
	wantDel := []int64{901, 902, 903}
	if len(up.deletes) != len(wantDel) {
		t.Fatalf("deletes = %v, want %v", up.deletes, wantDel)
	}
	for i := range wantDel {
		if up.deletes[i] != wantDel[i] {
			t.Errorf("deletes[%d] = %d, want %d", i, up.deletes[i], wantDel[i])
		}
	}
	assertTempFilesRemoved(t, down.paths)
}

// --- кеш и нецелевые элементы: download НЕ вызывается ---

func TestMediaResolver_Skips(t *testing.T) {
	cached := mtPhotoItem(5)
	cached.MediaFileID = "fid-cached"
	withURL := mtPhotoItem(6)
	withURL.MediaURL = "https://img.example/6.jpg"
	botapi := mtPhotoItem(7)
	botapi.Kind = "tg_botapi"
	noMedia := mtPhotoItem(8)
	noMedia.MediaKind = ""
	badID := mtPhotoItem(9)
	badID.ID = "abc"

	tests := []struct {
		name   string
		item   feed.Item
		budget int
	}{
		{"кеш message_state: MediaFileID заполнен", cached, 3},
		{"MediaURL от источника", withURL, 3},
		{"не tg_mtproto (botapi)", botapi, 3},
		{"без MediaKind", noMedia, 3},
		{"битый message id", badID, 3},
		{"бюджет исчерпан (0)", mtPhotoItem(10), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			down := &fakeDownloader{}
			up := &fakeUploader{}
			r := NewMediaResolver(down, up, discardMediaLog())
			items := []feed.Item{tt.item}
			used, err := r.Resolve(context.Background(), items, tt.budget)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if used != 0 || len(down.paths) != 0 || up.uploads != 0 {
				t.Errorf("used=%d downloads=%d uploads=%d, want all 0", used, len(down.paths), up.uploads)
			}
			if items[0].MediaFileID != tt.item.MediaFileID {
				t.Errorf("MediaFileID changed: %q → %q", tt.item.MediaFileID, items[0].MediaFileID)
			}
		})
	}
}

// --- ошибки: элемент без медиа, прогон жив, temp-файлы удалены ---

func TestMediaResolver_DownloadError(t *testing.T) {
	down := &fakeDownloader{failIDs: map[int]bool{2: true}}
	up := &fakeUploader{}
	r := NewMediaResolver(down, up, discardMediaLog())
	items := []feed.Item{mtPhotoItem(1), mtPhotoItem(2), mtPhotoItem(3)}

	used, err := r.Resolve(context.Background(), items, 3)
	if err != nil {
		t.Fatalf("Resolve вернул error — прогон не должен падать: %v", err)
	}
	if used != 2 {
		t.Errorf("used = %d, want 2", used)
	}
	if items[0].MediaFileID == "" || items[2].MediaFileID == "" {
		t.Errorf("соседние элементы потеряли медиа: %q / %q", items[0].MediaFileID, items[2].MediaFileID)
	}
	if items[1].MediaFileID != "" {
		t.Errorf("элемент с ошибкой download получил медиа %q", items[1].MediaFileID)
	}
	if len(up.deletes) != 2 {
		t.Errorf("deletes = %v, want 2", up.deletes)
	}
	assertTempFilesRemoved(t, down.paths)
}

func TestMediaResolver_UploadError(t *testing.T) {
	down := &fakeDownloader{}
	up := &fakeUploader{fileErr: errors.New("upload boom")}
	r := NewMediaResolver(down, up, discardMediaLog())
	items := []feed.Item{mtPhotoItem(1)}

	used, err := r.Resolve(context.Background(), items, 3)
	if err != nil || used != 0 {
		t.Fatalf("used=%d err=%v, want 0/nil", used, err)
	}
	if items[0].MediaFileID != "" {
		t.Errorf("MediaFileID = %q, want empty", items[0].MediaFileID)
	}
	if len(up.deletes) != 0 {
		t.Errorf("DeleteMessage вызван без успешного re-upload: %v", up.deletes)
	}
	assertTempFilesRemoved(t, down.paths)
}

func TestMediaResolver_DeleteErrorNonFatal(t *testing.T) {
	down := &fakeDownloader{}
	up := &fakeUploader{delErr: errors.New("delete boom")}
	r := NewMediaResolver(down, up, discardMediaLog())
	items := []feed.Item{mtPhotoItem(1)}

	used, err := r.Resolve(context.Background(), items, 3)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if used != 1 || items[0].MediaFileID != "fid-photo-1.jpg" {
		t.Errorf("used=%d fileID=%q — ошибка DeleteMessage не должна терять file_id", used, items[0].MediaFileID)
	}
	assertTempFilesRemoved(t, down.paths)
}
