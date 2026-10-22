// Тесты 9D-2a на telegram-уровне (план «Подэтап 9D» тест-кейс 6): диспетчер
// S:/M:/P:/PDEL в handleCallbackQuery — seam-маршрутизация S:/M: (data
// насквозь, ack раньше edit), не-owner/inaccessible → seam не вызывается,
// ошибка seam'а → ack «Не получилось открыть» без edit, P: → 9E: тот же
// seam-путь, что S:/M: (sendPhoto-контур и PDEL — photo_test.go), seam nil →
// деградация; inlineKeyboard маппит InlineButton.URL (Data игнорируется при
// непустом URL). Используются существующие seam-хелперы пакета: recorder,
// drillStub, cbQuery.

package telegram

import (
	"context"
	"errors"
	"testing"
)

func TestHandleCallbackQuery_Nav9D(t *testing.T) {
	const owner = int64(111)
	okResult := &CallbackResult{
		AckText: "готово",
		Edit: &EditMessage{
			Text:    "NAV VIEW",
			Buttons: [][]InlineButton{{{Text: "◀ Назад", Data: "T:42"}}},
		},
	}

	tests := []struct {
		name     string
		data     string
		from     int64
		access   bool
		nilSeam  bool
		res      *CallbackResult
		err      error
		wantLog  []string // порядок seam-записей recorder: ack раньше edit
		wantText string   // "" → edit не ожидается
		wantData string   // "" → seam не вызывается; иначе data как есть
	}{
		{name: "S: owner — ack(AckText) раньше edit", data: "S:42:7", from: owner, access: true,
			res: okResult, wantLog: []string{"ack:готово", "edit"}, wantText: "NAV VIEW", wantData: "S:42:7"},
		{name: "M: owner — тот же контур", data: "M:11", from: owner, access: true,
			res: okResult, wantLog: []string{"ack:готово", "edit"}, wantText: "NAV VIEW", wantData: "M:11"},
		{name: "legacy L2: по-прежнему уходит в seam", data: "L2:42", from: owner, access: true,
			res: okResult, wantLog: []string{"ack:готово", "edit"}, wantText: "NAV VIEW", wantData: "L2:42"},
		{name: "мусорные аргументы проходят в seam насквозь (telegram НЕ парсит)",
			data: "S:не-число:мусор", from: owner, access: true, res: okResult,
			wantLog: []string{"ack:готово", "edit"}, wantText: "NAV VIEW", wantData: "S:не-число:мусор"},
		{name: "ошибка seam → ack «Не получилось открыть», без edit", data: "S:1:1", from: owner, access: true,
			err: errors.New("db down"), wantLog: []string{"ack:" + drillDownErrorText}, wantText: "", wantData: "S:1:1"},
		{name: "не-owner → ack «Не ваш бот», seam не вызывается", data: "S:42:7", from: 999, access: true,
			res: okResult, wantLog: []string{"ack:" + cbNotOwnerText}, wantText: "", wantData: ""},
		{name: "M: inaccessible → только ack, seam не вызывается", data: "M:11", from: owner, access: false,
			res: okResult, wantLog: []string{"ack:"}, wantText: "", wantData: ""},
		{name: "P: — 9E: маршрутизуется в seam как S:/M: (sendPhoto-контур — photo_test.go)",
			data: "P:11", from: owner, access: true, res: okResult,
			wantLog: []string{"ack:готово", "edit"}, wantText: "NAV VIEW", wantData: "P:11"},
		{name: "P: inaccessible → только ack, seam не вызывается", data: "P:11", from: owner, access: false,
			res: okResult, wantLog: []string{"ack:"}, wantText: "", wantData: ""},
		{name: "S: seam nil → ack + edit «Drill-down недоступен.»", data: "S:42:7", from: owner, access: true,
			nilSeam: true, wantLog: []string{"ack:", "edit"}, wantText: drillDownUnavailableText, wantData: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recorder{}
			stub := &drillStub{res: tt.res, err: tt.err}
			b := &Bot{chatID: owner, log: discardLogger(), ackFn: rec.ack, editFn: rec.edit}
			if !tt.nilSeam {
				b.drillDown = stub.fn
			}

			b.handleCallbackQuery(context.Background(), cbQuery(tt.data, tt.from, tt.access))

			if len(rec.log) != len(tt.wantLog) {
				t.Fatalf("log = %v, want %v", rec.log, tt.wantLog)
			}
			for i := range tt.wantLog {
				if rec.log[i] != tt.wantLog[i] {
					t.Fatalf("log = %v, want %v", rec.log, tt.wantLog)
				}
			}
			if tt.wantText == "" {
				if len(rec.texts) != 0 {
					t.Fatalf("unexpected edits: %v", rec.texts)
				}
			} else if len(rec.texts) != 1 || rec.texts[0] != tt.wantText {
				t.Fatalf("edits = %v, want [%q]", rec.texts, tt.wantText)
			}
			if tt.wantData == "" {
				stub.mu.Lock()
				n := len(stub.datas)
				stub.mu.Unlock()
				if n != 0 {
					t.Errorf("seam вызван %d раз, want 0", n)
				}
			} else {
				stub.mu.Lock()
				got := append([]string(nil), stub.datas...)
				stub.mu.Unlock()
				if len(got) != 1 || got[0] != tt.wantData {
					t.Errorf("seam datas = %v, want [%q]", got, tt.wantData)
				}
			}
		})
	}
}

// URL-кнопка (план §4.6, тест-кейс 6): inlineKeyboard маппит InlineButton.URL
// в models.InlineKeyboardButton.URL; при непустом URL Data ИГНОРИруется
// (взаимоисключаемость — комментарий-контракт, не паника).
func TestInlineKeyboard_URLMapping(t *testing.T) {
	kb := inlineKeyboard([][]InlineButton{
		{{Text: "↗ Перейти", URL: "https://x.example/a", Data: "ignored:1"}},
		{{Text: "◀ Назад", Data: "S:1:2"}},
	})
	if kb == nil || len(kb.InlineKeyboard) != 2 {
		t.Fatalf("kb = %+v, want 2 rows", kb)
	}
	urlBtn := kb.InlineKeyboard[0][0]
	if urlBtn.URL != "https://x.example/a" {
		t.Errorf("URL = %q, want https://x.example/a", urlBtn.URL)
	}
	if urlBtn.CallbackData != "" {
		t.Errorf("Data must be IGNORED when URL is set, got %q", urlBtn.CallbackData)
	}
	dataBtn := kb.InlineKeyboard[1][0]
	if dataBtn.CallbackData != "S:1:2" || dataBtn.URL != "" {
		t.Errorf("data button = %+v, want CallbackData S:1:2, no URL", dataBtn)
	}
}
