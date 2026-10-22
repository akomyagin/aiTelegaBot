package mtproto

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
)

// PhotoDownloader — абстракция скачивания фото (план §4.5.1 п.1). *Client
// удовлетворяет её СТРУКТУРНО (DownloadPhoto); в тестах — фейк: реальный
// gotd-клиент не мокаем (существующая практика: клиентские методы mtproto
// тестируются на уровне нормализации).
type PhotoDownloader interface {
	DownloadPhoto(ctx context.Context, ref string, msgID int, destPath string) error
}

// PhotoUploader — минимальный потребительский интерфейс Bot API для
// re-upload (план §4.5.1 п.2). mtproto НЕ импортирует internal/telegram:
// telegram уже импортирует mtproto (PeerInfo), обратный импорт был бы циклом.
// *telegram.Bot удовлетворяет интерфейс СТРУКТУРНО, wiring — в app.go; та же
// инверсия зависимости, что у PeerCache (mtproto задаёт интерфейс-потребитель,
// app подключает реализацию).
type PhotoUploader interface {
	UploadPhoto(ctx context.Context, chatID, filePath, fileName string) (messageID int64, fileID string, err error)
	DeleteMessage(ctx context.Context, chatID string, messageID int64) error
}

// MediaResolver — боевой резолвер медиа подэтапа 9E (план §4.5.1): фото
// приватного канала нельзя показать ни по URL, ни чужим file_id, поэтому для
// tg_mtproto-элементов с MediaKind="photo" и пустым media-полем выполняется
// download → re-upload ботом (owner-чат, без caption) → file_id → СРАЗУ
// deleteMessage служебного сообщения (пользователь видит только финальный
// дайджест).
//
// Реализует контракт digest.MediaResolver (см. digest/pipeline.go):
//   - мутирует items IN PLACE (MediaFileID); элементы уже с media-полем
//     (кеш message_state заполнил pipeline) пропускаются — download НЕ
//     вызывается («кеш по dedup_key: re-upload не повторяется», §4.5);
//   - budget — лимит re-upload'ов ЭТОГО прогона (счётчик общий на run живёт
//     в pipeline, план §4.8.1); сверх бюджета медиа не показываем — поле
//     остаётся пустым, L3 рендерится без кнопки «🖼» (деградация §4.5.1),
//     log.Info;
//   - ошибка любого шага (download, re-upload, битый id) — log.Warn,
//     элемент без медиа, error НАРУЖУ НЕ возвращается: прогон дайджеста не
//     должен падать из-за одного фото (§4.5.1);
//   - storage НЕ трогает: чтение кеша и запись file_id (message_state +
//     digest_items) владеет pipeline — один владелец storage.
type MediaResolver struct {
	down PhotoDownloader
	up   PhotoUploader
	log  *slog.Logger
}

// NewMediaResolver собирает резолвер. log nil → slog.Default().
func NewMediaResolver(down PhotoDownloader, up PhotoUploader, log *slog.Logger) *MediaResolver {
	if log == nil {
		log = slog.Default()
	}
	return &MediaResolver{down: down, up: up, log: log}
}

// Resolve implements digest.MediaResolver. Порядок обработки — порядок items
// (collect-порядок, план §6: никакой map-итерации).
func (r *MediaResolver) Resolve(ctx context.Context, items []feed.Item, budget int) (int, error) {
	used := 0
	for i := range items {
		it := &items[i]
		if it.Kind != "tg_mtproto" || it.MediaKind != "photo" {
			continue // не наш случай: медиа только у tg_mtproto-фото (9E-2a)
		}
		if it.MediaFileID != "" || it.MediaURL != "" {
			continue // кеш message_state (заполнил pipeline) — download не нужен
		}
		if budget-used <= 0 {
			r.log.Info("mtproto media: re-upload budget exhausted; item without photo",
				"key", it.DedupKey(), "budget", budget)
			continue
		}
		msgID, err := strconv.Atoi(it.ID)
		if err != nil || msgID <= 0 {
			r.log.Warn("mtproto media: bad message id; item without photo",
				"key", it.DedupKey(), "id", it.ID)
			continue
		}
		fileID, err := r.resolveOne(ctx, it.Source, msgID)
		if err != nil {
			r.log.Warn("mtproto media: resolve failed; item without photo",
				"key", it.DedupKey(), "err", err)
			continue
		}
		it.MediaFileID = fileID
		used++
	}
	return used, nil
}

// resolveOne — полный цикл ОДНОГО фото (§4.5.1): temp-файл → DownloadPhoto
// (стриминг на диск) → UploadPhoto (бот, owner-чат) → file_id → DeleteMessage
// служебного сообщения. Temp-файл создаётся os.CreateTemp в os.TempDir() и
// удаляется defer'ом при любом исходе — крах не оставляет мусор (temp-каталог
// дополнительно чистится ОС).
func (r *MediaResolver) resolveOne(ctx context.Context, ref string, msgID int) (string, error) {
	tmp, err := os.CreateTemp("", "aitelega-photo-*.jpg")
	if err != nil {
		return "", fmt.Errorf("temp file: %w", err)
	}
	path := tmp.Name()
	_ = tmp.Close() // DownloadPhoto сам открывает путь на запись (os.Create)
	defer func() { _ = os.Remove(path) }()

	if err := r.down.DownloadPhoto(ctx, ref, msgID, path); err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	svcID, fileID, err := r.up.UploadPhoto(ctx, "", path, fmt.Sprintf("photo-%d.jpg", msgID))
	if err != nil {
		// «no photo в ответе» приходит с id УЖЕ отправленного служебного
		// сообщения (uploadPhotoResult) — удаляем, чтобы не утекало в
		// owner-чат (ревью 9E, фикс 6). Транспортная ошибка (svcID
		// недоступен) — удалять нечего, поведение прежнее.
		if svcID > 0 {
			if derr := r.up.DeleteMessage(ctx, "", svcID); derr != nil {
				r.log.Warn("mtproto media: delete service message after failed upload",
					"svc_id", svcID, "err", derr)
			}
		}
		return "", fmt.Errorf("re-upload: %w", err)
	}
	// Служебное сообщение удаляется СРАЗУ после re-upload (§4.5.1 п.2).
	// Ошибка удаления не фатальна: file_id уже получен, единичное служебное
	// сообщение в owner-чате — приемлемая утечка (log).
	if derr := r.up.DeleteMessage(ctx, "", svcID); derr != nil {
		r.log.Warn("mtproto media: delete service message failed", "err", derr)
	}
	if fileID == "" {
		return "", errors.New("re-upload returned empty file_id")
	}
	return fileID, nil
}
