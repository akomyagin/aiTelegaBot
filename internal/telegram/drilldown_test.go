// Stage 8 D-1 тесты (план §D, тест-кейсы D на telegram-уровне): диспетчер
// L1:/L2:/L3: — DrillDown seam (data проходит насквозь без парсинга, порядок
// ack-раньше-edit, ошибка seam'а → ack без edit, nil-деградация без паники,
// not-owner/inaccessible → seam не вызывается) и alias L1: → viewer-путь
// (9D: клавиатуру T:/L1:-view полностью собирает viewer).

package telegram

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// drillStub — фейковый DrillDown seam: записывает data как есть и возвращает
// заданный результат (образец viewerStub из topic_view_test.go).
type drillStub struct {
	mu    sync.Mutex
	datas []string
	res   *CallbackResult
	err   error
}

func (d *drillStub) fn(ctx context.Context, data string) (*CallbackResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.datas = append(d.datas, data)
	return d.res, d.err
}

func TestHandleCallbackQuery_DrillDown(t *testing.T) {
	const owner = int64(111)
	okResult := &CallbackResult{
		AckText: "готово",
		Edit: &EditMessage{
			Text:    "L2 VIEW",
			Buttons: [][]InlineButton{{{Text: "◀ Назад", Data: "L1:42"}}},
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
		wantLog  []string // порядок seam-записей: ack раньше edit (recorder)
		wantText string   // "" → edit не ожидается
		wantKb   string   // "" → не проверяем; "nav" → ряд [◀ Назад L1:42]; "empty" → non-nil пустой
		wantData string   // "" → seam не вызывается; иначе data как есть
	}{
		{name: "L2: успех — ack(AckText) раньше edit, markup из Buttons",
			data: "L2:42", from: owner, access: true, res: okResult,
			wantLog: []string{"ack:готово", "edit"}, wantText: "L2 VIEW", wantKb: "nav", wantData: "L2:42"},
		{name: "L3: nil Edit → только ack",
			data: "L3:42:7", from: owner, access: true, res: &CallbackResult{AckText: ""},
			wantLog: []string{"ack:"}, wantText: "", wantData: "L3:42:7"},
		{name: "мусорные аргументы проходят в seam насквозь (telegram НЕ парсит)",
			data: "L3:не-число:мусор", from: owner, access: true, res: okResult,
			wantLog: []string{"ack:готово", "edit"}, wantText: "L2 VIEW", wantData: "L3:не-число:мусор"},
		{name: "ошибка seam → ack «Не получилось открыть», без edit",
			data: "L2:42", from: owner, access: true, err: errors.New("db down"),
			wantLog: []string{"ack:" + drillDownErrorText}, wantText: "", wantData: "L2:42"},
		{name: "seam nil → ack + edit «недоступен», пустая клавиатура-замена",
			data: "L3:42:0", from: owner, access: true, nilSeam: true,
			wantLog: []string{"ack:", "edit"}, wantText: drillDownUnavailableText, wantKb: "empty"},
		{name: "не-owner → ack «Не ваш бот», seam не вызывается",
			data: "L2:42", from: 999, access: true, res: okResult,
			wantLog: []string{"ack:" + cbNotOwnerText}, wantText: "", wantData: ""},
		{name: "inaccessible → только ack, seam не вызывается",
			data: "L2:42", from: owner, access: false, res: okResult,
			wantLog: []string{"ack:"}, wantText: "", wantData: ""},
		{name: "пустые Buttons → non-nil пустой markup (старая клавиатура убирается)",
			data: "L2:42", from: owner, access: true,
			res:     &CallbackResult{Edit: &EditMessage{Text: "PLAIN"}},
			wantLog: []string{"ack:", "edit"}, wantText: "PLAIN", wantKb: "empty", wantData: "L2:42"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recorder{}
			b := &Bot{chatID: owner, log: discardLogger(), ackFn: rec.ack, editFn: rec.edit}
			d := &drillStub{res: tt.res, err: tt.err}
			if !tt.nilSeam {
				b.drillDown = d.fn
			}

			b.handleCallbackQuery(context.Background(), cbQuery(tt.data, tt.from, tt.access))

			if len(rec.log) != len(tt.wantLog) {
				t.Fatalf("порядок seams = %v, want %v", rec.log, tt.wantLog)
			}
			for i := range rec.log {
				if rec.log[i] != tt.wantLog[i] {
					t.Fatalf("порядок seams = %v, want %v", rec.log, tt.wantLog)
				}
			}
			if tt.wantText == "" {
				if len(rec.texts) != 0 {
					t.Fatalf("неожиданные edits: %v", rec.texts)
				}
			} else if len(rec.texts) != 1 || rec.texts[0] != tt.wantText {
				t.Fatalf("edits = %v, want [%q]", rec.texts, tt.wantText)
			}
			if tt.wantData == "" {
				if len(d.datas) != 0 {
					t.Errorf("seam вызван с %v, want 0 вызовов", d.datas)
				}
			} else if len(d.datas) != 1 || d.datas[0] != tt.wantData {
				t.Errorf("seam datas = %v, want [%q] как есть", d.datas, tt.wantData)
			}
			if tt.wantKb != "" && len(rec.kbs) == 1 {
				kb := rec.kbs[0]
				if kb == nil {
					t.Fatal("markup = nil, want non-nil (клавиатура ЗАМЕНЯется)")
				}
				switch tt.wantKb {
				case "empty":
					if len(kb.InlineKeyboard) != 0 {
						t.Errorf("markup = %+v, want пустой (убрать старые кнопки)", kb)
					}
				case "nav":
					if len(kb.InlineKeyboard) != 1 || len(kb.InlineKeyboard[0]) != 1 ||
						kb.InlineKeyboard[0][0].Text != "◀ Назад" ||
						kb.InlineKeyboard[0][0].CallbackData != "L1:42" {
						t.Errorf("markup = %+v, want ряд [◀ Назад L1:42]", kb)
					}
				}
			}
		})
	}
}

// «L1:<digestID>» — alias T:-пути: тот же viewer seam (ViewKindTopic), тот же
// owner/ack/inaccessible-контур; drillDown не вызывается. Клавиатура — как её
// собрал viewer (9D: telegram ничего не докомпановывает, «Подробнее →» нет).
func TestHandleCallbackQuery_L1IsTopicViewAlias(t *testing.T) {
	const owner = int64(111)
	view := DigestView{
		Text:    "L1 BODY",
		Buttons: [][]InlineButton{{{Text: "◀ К оглавлению", Data: "TOC:77"}}},
	}
	rec := &recorder{}
	v := &viewerStub{view: view, ok: true}
	d := &drillStub{res: &CallbackResult{}}
	b := &Bot{chatID: owner, log: discardLogger(), ackFn: rec.ack, editFn: rec.edit,
		digestViewer: v.fn, drillDown: d.fn}

	b.handleCallbackQuery(context.Background(), cbQuery("L1:42", owner, true))

	if v.calls != 1 || v.reqs[0].Kind != ViewKindTopic || v.reqs[0].ID != 42 {
		t.Fatalf("viewer reqs = %+v, want один topic/42", v.reqs)
	}
	if len(d.datas) != 0 {
		t.Errorf("L1: не должен доходить до drillDown seam, got %v", d.datas)
	}
	if len(rec.texts) != 1 || rec.texts[0] != "L1 BODY" {
		t.Fatalf("edits = %v, want [L1 BODY]", rec.texts)
	}
	kb := rec.kbs[0]
	if kb == nil || len(kb.InlineKeyboard) != 1 || len(kb.InlineKeyboard[0]) != 1 {
		t.Fatalf("markup = %+v, want 1 ряд [◀ К оглавлению TOC:77]", kb)
	}
	if kb.InlineKeyboard[0][0].Text != "◀ К оглавлению" || kb.InlineKeyboard[0][0].CallbackData != "TOC:77" {
		t.Errorf("button[0] = %+v, want {◀ К оглавлению, TOC:77}", kb.InlineKeyboard[0][0])
	}
	// view.Buttons не мутируется in-place (viewer может переиспользовать срезы).
	if len(view.Buttons[0]) != 1 || view.Buttons[0][0].Data != "TOC:77" {
		t.Errorf("view.Buttons мутирован: %+v", view.Buttons)
	}
}
