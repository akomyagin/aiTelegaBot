// Тесты Этапа 9E ЧАСТЬ 1 (план §4.5.2, тест-кейсы 2/3/6): SendPhoto/
// DeleteMessage через seams sendPhotoFn/deleteMsgFn (file_id-путь, URL-путь
// http/https в любом регистре, ошибка seam → error, без транспорта → error,
// защитная резка caption ≤1024 байт на границе рун), активация
// CallbackResult.Send в handleDrillDown (sendPhoto в owner-чат, edit НЕ
// вызывается; ошибка SendPhoto → ack «Не удалось показать фото») и ветка
// «PDEL» (deleteMessage того же сообщения + ack; inaccessible → только ack).
// Фейки вместо сети; общие хелперы пакета: recorder, drillStub, cbQuery.

package telegram

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/go-telegram/bot/models"
)

// photoRec — фейковый транспорт фото: пишет порядок вызовов (send/delete),
// параметры sendPhoto и возвращает заданные результаты.
type photoRec struct {
	log      []string
	chatIDs  []int64
	photos   []models.InputFile
	captions []string
	kbs      []*models.InlineKeyboardMarkup
	deleted  []int64
	sendID   int64
	sendErr  error
	delErr   error
}

func (r *photoRec) sendPhoto(ctx context.Context, chatID int64, photo models.InputFile, caption string, kb *models.InlineKeyboardMarkup) (int64, error) {
	r.log = append(r.log, "photo")
	r.chatIDs = append(r.chatIDs, chatID)
	r.photos = append(r.photos, photo)
	r.captions = append(r.captions, caption)
	r.kbs = append(r.kbs, kb)
	return r.sendID, r.sendErr
}

func (r *photoRec) deleteMsg(ctx context.Context, chatID int64, messageID int64) error {
	r.log = append(r.log, "delete")
	r.deleted = append(r.deleted, messageID)
	return r.delErr
}

func TestSendPhoto_Transport(t *testing.T) {
	tests := []struct {
		name       string
		chatID     string
		mediaRef   string
		sendErr    error
		wantErr    bool
		wantChatID int64
		wantID     int64
	}{
		{name: "file_id → InputFileString{Data}, owner-чат", mediaRef: "AgADBAADxyz", wantChatID: 111, wantID: 55},
		{name: "https URL → тот же строковый транспорт (URL качают серверы Telegram)",
			mediaRef: "https://a.example/p.jpg", wantChatID: 111, wantID: 55},
		{name: "HTTP:// в верхнем регистре — тоже URL-ветка (схема case-insensitive)",
			mediaRef: "HTTP://A.EXAMPLE/p.jpg", wantChatID: 111, wantID: 55},
		{name: "явный chatID", chatID: "222", mediaRef: "AgAD", wantChatID: 222, wantID: 55},
		{name: "ошибка seam → error наружу (обёрнутая)", mediaRef: "AgAD", sendErr: errors.New("bot api down"), wantErr: true},
		{name: "пустой mediaRef → error, seam не вызывается", mediaRef: "   ", wantErr: true},
		{name: "мусорный chatID → error", chatID: "не-число", mediaRef: "AgAD", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &photoRec{sendID: 55, sendErr: tt.sendErr}
			b := &Bot{chatID: 111, log: discardLogger(), sendPhotoFn: rec.sendPhoto}

			id, err := b.SendPhoto(context.Background(), tt.chatID, tt.mediaRef, "cap",
				[][]InlineButton{{{Text: "✖ Закрыть", Data: "PDEL"}}})

			if tt.wantErr {
				if err == nil {
					t.Fatalf("SendPhoto(%q) = %d, want error", tt.mediaRef, id)
				}
				return
			}
			if err != nil {
				t.Fatalf("SendPhoto: %v", err)
			}
			if id != tt.wantID {
				t.Errorf("messageID = %d, want %d", id, tt.wantID)
			}
			if len(rec.chatIDs) != 1 || rec.chatIDs[0] != tt.wantChatID {
				t.Fatalf("chatIDs = %v, want [%d]", rec.chatIDs, tt.wantChatID)
			}
			fs, ok := rec.photos[0].(*models.InputFileString)
			if !ok || fs.Data != tt.mediaRef {
				t.Errorf("photo = %#v, want InputFileString{%q} (v1.22: file_id и URL — строка)", rec.photos[0], tt.mediaRef)
			}
			if rec.captions[0] != "cap" {
				t.Errorf("caption = %q, want cap", rec.captions[0])
			}
			kb := rec.kbs[0]
			if kb == nil || len(kb.InlineKeyboard) != 1 ||
				kb.InlineKeyboard[0][0].Text != "✖ Закрыть" || kb.InlineKeyboard[0][0].CallbackData != "PDEL" {
				t.Errorf("kb = %+v, want ряд [✖ Закрыть PDEL]", kb)
			}
			if len("PDEL") > 64 { // callback_data ≤64 байт (план §6)
				t.Error("PDEL > 64 байт")
			}
		})
	}

	t.Run("без seam и без api → error, НЕ silent no-op", func(t *testing.T) {
		b := &Bot{chatID: 111, log: discardLogger()}
		if _, err := b.SendPhoto(context.Background(), "", "AgAD", "", nil); err == nil {
			t.Error("want error without transport")
		}
	})
}

// Тест-кейс 2 плана: caption >1024 байт (многобайтные руны — кириллица/эмодзи)
// → защитная резка SendPhoto на границе рун, assert ≤1024 байт.
func TestSendPhoto_CaptionTruncation(t *testing.T) {
	tests := []struct {
		name    string
		caption string
	}{
		{"ascii", "<b>" + strings.Repeat("x", 2000) + "</b>"},
		{"кириллица", strings.Repeat("фыв", 500)}, // 1500 рун × 2 байта = 3000
		{"эмодзи", strings.Repeat("🖼", 400)},      // 400 рун × 4 байта = 1600
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &photoRec{sendID: 1}
			b := &Bot{chatID: 111, log: discardLogger(), sendPhotoFn: rec.sendPhoto}
			if _, err := b.SendPhoto(context.Background(), "", "AgAD", tt.caption, nil); err != nil {
				t.Fatalf("SendPhoto: %v", err)
			}
			got := rec.captions[0]
			if len(got) > photoCaptionMaxBytes {
				t.Fatalf("caption = %d байт > %d", len(got), photoCaptionMaxBytes)
			}
			if !strings.HasSuffix(got, "…") {
				t.Error("обрезанный caption должен заканчиваться «…»")
			}
			if !utf8.ValidString(got) {
				t.Error("caption обрезан НЕ на границе рун")
			}
		})
	}
	t.Run("короткий caption не трогается", func(t *testing.T) {
		rec := &photoRec{sendID: 1}
		b := &Bot{chatID: 111, log: discardLogger(), sendPhotoFn: rec.sendPhoto}
		if _, err := b.SendPhoto(context.Background(), "", "AgAD", "ок", nil); err != nil {
			t.Fatalf("SendPhoto: %v", err)
		}
		if rec.captions[0] != "ок" {
			t.Errorf("caption = %q, want «ок» без изменений", rec.captions[0])
		}
	})
}

func TestDeleteMessage_Transport(t *testing.T) {
	rec := &photoRec{}
	b := &Bot{chatID: 111, log: discardLogger(), deleteMsgFn: rec.deleteMsg}
	if err := b.DeleteMessage(context.Background(), "", 77); err != nil {
		t.Fatalf("DeleteMessage(owner): %v", err)
	}
	if err := b.DeleteMessage(context.Background(), "222", 88); err != nil {
		t.Fatalf("DeleteMessage(222): %v", err)
	}
	if len(rec.deleted) != 2 || rec.deleted[0] != 77 || rec.deleted[1] != 88 {
		t.Errorf("deleted = %v, want [77 88]", rec.deleted)
	}

	rec.delErr = errors.New("gone")
	if err := b.DeleteMessage(context.Background(), "", 99); err == nil {
		t.Error("ошибка seam должна возвращаться наружу")
	}
	noTransport := &Bot{chatID: 111, log: discardLogger()}
	if err := noTransport.DeleteMessage(context.Background(), "", 1); err == nil {
		t.Error("без seam и api → error, НЕ silent no-op")
	}
	if err := b.DeleteMessage(context.Background(), "мусор", 1); err == nil {
		t.Error("мусорный chatID → error")
	}
}

// Тест-кейс 3 плана: handleDrillDown c Send≠nil → sendPhotoFn вызван, editFn
// НЕ вызван; ошибка SendPhoto → ack «Не удалось показать фото» без edit;
// DeleteMessageID>0 → deleteMsgFn; PDEL → deleteMsgFn c id из q.Message + ack;
// PDEL inaccessible → только ack; ошибка delete → ack «Не удалось закрыть».
func TestHandleDrillDown_Send9E(t *testing.T) {
	const owner = int64(111)
	send := &SendAction{
		PhotoMediaRef: "AgADBAAD",
		PhotoCaption:  "<b>Заголовок</b>",
		PhotoButtons:  [][]InlineButton{{{Text: "✖ Закрыть", Data: "PDEL"}}},
	}

	tests := []struct {
		name       string
		data       string
		access     bool
		res        *CallbackResult
		seamErr    error
		sendErr    error
		delErr     error
		delID      int64 // >0 → SendAction.DeleteMessageID
		wantLog    []string
		wantPhoto  int // число вызовов sendPhotoFn
		wantDelete int // число вызовов deleteMsgFn
	}{
		{name: "Send≠nil → sendPhoto в owner-чат, ack(AckText) после, edit НЕ вызван",
			data: "P:11", access: true, res: &CallbackResult{AckText: "ок", Send: send},
			wantLog: []string{"photo", "ack:ок"}, wantPhoto: 1},
		{name: "Send≠nil и AckText пуст → ack «»", data: "P:11", access: true,
			res: &CallbackResult{Send: send}, wantLog: []string{"photo", "ack:"}, wantPhoto: 1},
		{name: "ошибка SendPhoto → ack «Не удалось показать фото», edit не трогаем",
			data: "P:11", access: true, res: &CallbackResult{Send: send}, sendErr: errors.New("5MB limit"),
			wantLog: []string{"photo", "ack:" + photoSendFailedText}, wantPhoto: 1},
		{name: "Send c DeleteMessageID>0 → deleteMessage после sendPhoto",
			data: "P:11", access: true, res: &CallbackResult{Send: send}, delID: 4242,
			wantLog: []string{"photo", "delete", "ack:"}, wantPhoto: 1, wantDelete: 1},
		{name: "ошибка seam → ack «Не получилось открыть», sendPhoto НЕ вызван",
			data: "P:11", access: true, seamErr: errors.New("media_ref пуст"),
			wantLog: []string{"ack:" + drillDownErrorText}},
		{name: "PDEL → deleteMessage(id из q.Message) + ack «»",
			data: "PDEL", access: true, delErr: nil,
			wantLog: []string{"delete", "ack:"}, wantDelete: 1},
		{name: "PDEL inaccessible → только ack, delete не вызывается",
			data: "PDEL", access: false, wantLog: []string{"ack:"}},
		{name: "PDEL ошибка delete → ack «Не удалось закрыть»",
			data: "PDEL", access: true, delErr: errors.New("api"),
			wantLog: []string{"delete", "ack:" + photoDeleteFailedText}, wantDelete: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Общий order-лог всех seam-вызовов (порядок photo/delete/ack/edit
			// в одном срезе — handler однопоточен).
			var order []string
			prec := &photoRec{sendID: 900, sendErr: tt.sendErr, delErr: tt.delErr}
			stub := &drillStub{res: tt.res, err: tt.seamErr}
			b := &Bot{chatID: owner, log: discardLogger(), drillDown: stub.fn}
			b.sendPhotoFn = func(ctx context.Context, chatID int64, photo models.InputFile, caption string, kb *models.InlineKeyboardMarkup) (int64, error) {
				order = append(order, "photo")
				return prec.sendPhoto(ctx, chatID, photo, caption, kb)
			}
			b.deleteMsgFn = func(ctx context.Context, chatID int64, messageID int64) error {
				order = append(order, "delete")
				return prec.deleteMsg(ctx, chatID, messageID)
			}
			b.ackFn = func(ctx context.Context, id, text string) error {
				order = append(order, "ack:"+text)
				return nil
			}
			b.editFn = func(ctx context.Context, chatID int64, msgID int, text string, kb *models.InlineKeyboardMarkup) error {
				order = append(order, "edit")
				return nil
			}
			if tt.delID > 0 {
				s := *send
				s.DeleteMessageID = tt.delID
				b.drillDown = (&drillStub{res: &CallbackResult{Send: &s}, err: tt.seamErr}).fn
			}

			b.handleCallbackQuery(context.Background(), cbQuery(tt.data, owner, tt.access))

			if strings.Join(order, "|") != strings.Join(tt.wantLog, "|") {
				t.Fatalf("порядок seam-вызовов = %v, want %v", order, tt.wantLog)
			}
			if n := len(prec.photos); n != tt.wantPhoto {
				t.Errorf("sendPhoto вызван %d раз, want %d", n, tt.wantPhoto)
			}
			if n := len(prec.deleted); n != tt.wantDelete {
				t.Errorf("deleteMessage вызван %d раз, want %d", n, tt.wantDelete)
			}
			if tt.wantPhoto == 1 {
				if len(prec.chatIDs) != 1 || prec.chatIDs[0] != owner {
					t.Errorf("sendPhoto chatID = %v, want owner %d", prec.chatIDs, owner)
				}
				fs, ok := prec.photos[0].(*models.InputFileString)
				if !ok || fs.Data != "AgADBAAD" {
					t.Errorf("photo = %#v, want file_id AgADBAAD", prec.photos[0])
				}
			}
			if tt.wantDelete == 1 && len(prec.deleted) == 1 && tt.data == "PDEL" && prec.deleted[0] != 9 {
				t.Errorf("PDEL delete id = %d, want 9 (cbQuery message id)", prec.deleted[0])
			}
			if tt.delID > 0 && len(prec.deleted) == 1 && prec.deleted[0] != tt.delID {
				t.Errorf("delete id = %d, want %d", prec.deleted[0], tt.delID)
			}
		})
	}
}
