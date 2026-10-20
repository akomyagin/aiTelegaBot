// Stage 8 C tests (план §C, тест-кейс 6): DeliverWithButtons over the sendFn
// seam and the T:/TOC: callback contour over ackFn/editFn seams. Все данные
// view — из SQLite через viewer (после «рестарта» новый Bot рендерит из БД).

package telegram

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/go-telegram/bot/models"

	"github.com/akomyagin/aiTelegaBot/internal/storage"
)

// --- DeliverWithButtons ---

func TestDeliverWithButtons_ChatIDSemantics(t *testing.T) {
	type sent struct {
		chatID int64
		text   string
		kb     *models.InlineKeyboardMarkup
	}
	var mu sync.Mutex
	var got []sent
	b := &Bot{
		chatID: 111,
		log:    discardLogger(),
		sendFn: func(ctx context.Context, chatID int64, text string, kb *models.InlineKeyboardMarkup) error {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, sent{chatID, text, kb})
			return nil
		},
	}
	ctx := context.Background()
	buttons := [][]InlineButton{
		{{Text: "Открыть: A", Data: "T:1"}},
		{{Text: "Открыть: B", Data: "T:2"}},
	}

	// "" → owner chat.
	if err := b.DeliverWithButtons(ctx, "", "TOC", buttons); err != nil {
		t.Fatalf("DeliverWithButtons(\"\"): %v", err)
	}
	if got[0].chatID != 111 {
		t.Errorf("chatID = %d, want owner 111", got[0].chatID)
	}
	// Конвертация кнопок в markup: ряд на кнопку, Text/CallbackData.
	kb := got[0].kb
	if kb == nil || len(kb.InlineKeyboard) != 2 || len(kb.InlineKeyboard[0]) != 1 {
		t.Fatalf("markup = %+v, want 2 rows × 1", kb)
	}
	if kb.InlineKeyboard[0][0].Text != "Открыть: A" || kb.InlineKeyboard[0][0].CallbackData != "T:1" {
		t.Errorf("button = %+v, want {Открыть: A, T:1}", kb.InlineKeyboard[0][0])
	}
	// Явный chatID.
	if err := b.DeliverWithButtons(ctx, "222", "x", nil); err != nil {
		t.Fatalf("DeliverWithButtons(222): %v", err)
	}
	if got[1].chatID != 222 {
		t.Errorf("chatID = %d, want 222", got[1].chatID)
	}
	if got[1].kb != nil {
		t.Errorf("empty buttons must not set a markup, got %+v", got[1].kb)
	}
	// Невалидный chatID → error (seam не вызывается).
	before := len(got)
	if err := b.DeliverWithButtons(ctx, "abc", "x", nil); err == nil {
		t.Error("invalid chatID: want error")
	} else if len(got) != before {
		t.Error("invalid chatID: seam must not be called")
	}
}

func TestDeliverWithButtons_Errors(t *testing.T) {
	ctx := context.Background()
	// Ошибка seam → ошибка наружу (Run обязан узнать о сбое доставки).
	boom := &Bot{chatID: 1, log: discardLogger(),
		sendFn: func(ctx context.Context, chatID int64, text string, kb *models.InlineKeyboardMarkup) error {
			return errors.New("network down")
		}}
	if err := boom.DeliverWithButtons(ctx, "", "x", nil); err == nil ||
		!strings.Contains(err.Error(), "network down") {
		t.Errorf("seam error = %v, want it surfaced", err)
	}
	// api==nil && sendFn==nil → error, НЕ тихий no-op.
	dead := &Bot{chatID: 1, log: discardLogger()}
	if err := dead.DeliverWithButtons(ctx, "", "x", nil); err == nil {
		t.Error("no transport: want error")
	}
}

// --- T:/TOC: callbacks ---

type viewerStub struct {
	mu    sync.Mutex
	reqs  []DigestViewRequest
	view  DigestView
	ok    bool
	err   error
	calls int
}

func (v *viewerStub) fn(ctx context.Context, req DigestViewRequest) (DigestView, bool, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls++
	v.reqs = append(v.reqs, req)
	return v.view, v.ok, v.err
}

type recorder struct {
	mu    sync.Mutex
	log   []string
	texts []string
	kbs   []*models.InlineKeyboardMarkup
}

func (r *recorder) ack(ctx context.Context, id, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.log = append(r.log, "ack:"+text)
	return nil
}

func (r *recorder) edit(ctx context.Context, chatID int64, msgID int, text string, kb *models.InlineKeyboardMarkup) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.log = append(r.log, "edit")
	r.texts = append(r.texts, text)
	r.kbs = append(r.kbs, kb)
	return nil
}

func cbQuery(data string, fromID int64, accessible bool) *models.CallbackQuery {
	q := &models.CallbackQuery{
		ID:   "cb-1",
		From: models.User{ID: fromID},
		Data: data,
	}
	if accessible {
		q.Message.Message = &models.Message{ID: 9, Chat: models.Chat{ID: fromID}}
	}
	return q
}

func TestHandleCallbackQuery_DigestViews(t *testing.T) {
	const owner = int64(111)
	view := DigestView{
		Text:    "L1 BODY",
		Buttons: [][]InlineButton{{{Text: "◀ К оглавлению", Data: "TOC:77"}}},
	}

	tests := []struct {
		name      string
		data      string
		from      int64
		access    bool
		nilViewer bool
		vOk       bool
		vErr      error
		wantAck   string // текст ack'а
		wantEdit  string // "" → edit не ожидается
		wantKind  string // вид запроса viewer'у ("" → viewer не вызывается)
		wantBtns  bool
	}{
		{name: "T: success (ack→edit, markup)", data: "T:42", from: owner, access: true,
			vOk: true, wantAck: "ack:", wantEdit: "L1 BODY", wantKind: ViewKindTopic, wantBtns: true},
		{name: "TOC: routes to the toc kind", data: "TOC:77", from: owner, access: true,
			vOk: true, wantAck: "ack:", wantEdit: "L1 BODY", wantKind: ViewKindTOC},
		{name: "not owner: ack only", data: "T:42", from: 999, access: true,
			vOk: true, wantAck: "ack:" + cbNotOwnerText, wantEdit: "", wantKind: ""},
		{name: "inaccessible: ack only", data: "T:42", from: owner, access: false,
			vOk: true, wantAck: "ack:", wantEdit: "", wantKind: ""},
		{name: "bad arg", data: "T:abc", from: owner, access: true,
			vOk: true, wantAck: "ack:", wantEdit: digestViewBadIDText, wantKind: ""},
		{name: "viewer nil", data: "T:42", from: owner, access: true, nilViewer: true,
			wantAck: "ack:", wantEdit: digestViewUnavailableText, wantKind: ""},
		{name: "not found", data: "T:42", from: owner, access: true,
			vOk: false, wantAck: "ack:", wantEdit: digestViewNotFoundText, wantKind: ViewKindTopic},
		{name: "viewer error", data: "TOC:77", from: owner, access: true,
			vOk: false, vErr: errors.New("db down"), wantAck: "ack:", wantEdit: digestViewErrorText, wantKind: ViewKindTOC},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recorder{}
			v := &viewerStub{view: view, ok: tt.vOk, err: tt.vErr}
			b := &Bot{chatID: owner, log: discardLogger(), ackFn: rec.ack, editFn: rec.edit}
			if !tt.nilViewer {
				b.digestViewer = v.fn
			}

			b.handleCallbackQuery(context.Background(), cbQuery(tt.data, tt.from, tt.access))

			if len(rec.log) == 0 || rec.log[0] != tt.wantAck {
				t.Fatalf("ack = %v, want first %q", rec.log, tt.wantAck)
			}
			if tt.wantEdit == "" {
				if len(rec.texts) != 0 {
					t.Fatalf("unexpected edits: %v", rec.texts)
				}
			} else {
				if len(rec.texts) != 1 || rec.texts[0] != tt.wantEdit {
					t.Fatalf("edits = %v, want [%q]", rec.texts, tt.wantEdit)
				}
				if len(rec.log) != 2 || rec.log[1] != "edit" {
					t.Errorf("order = %v, want [ack edit]", rec.log)
				}
			}
			if tt.wantKind == "" {
				if v.calls != 0 {
					t.Errorf("viewer called %d times, want 0", v.calls)
				}
			} else {
				if v.calls != 1 || v.reqs[0].Kind != tt.wantKind {
					t.Fatalf("viewer reqs = %+v, want one %q", v.reqs, tt.wantKind)
				}
				var wantID int64 = 42
				if tt.wantKind == ViewKindTOC {
					wantID = 77
				}
				if v.reqs[0].ID != wantID {
					t.Errorf("viewer ID = %d, want %d", v.reqs[0].ID, wantID)
				}
			}
			if tt.wantBtns && len(rec.kbs) == 1 {
				kb := rec.kbs[0]
				// D-1 (граница C/D): telegram докомпановывает к L1-view кнопку
				// «Подробнее →» (L2:<digestID>) РЯДОМ с кнопкой viewer'а
				// «◀ К оглавлению» — тот же ряд, первой. Единственное
				// отличие golden'а от C: добавленная кнопка.
				if kb == nil || len(kb.InlineKeyboard) != 1 || len(kb.InlineKeyboard[0]) != 2 ||
					kb.InlineKeyboard[0][0].Text != drillDownMoreLabel ||
					kb.InlineKeyboard[0][0].CallbackData != "L2:42" ||
					kb.InlineKeyboard[0][1].CallbackData != "TOC:77" {
					t.Errorf("edit markup = %+v, want ряд [Подробнее → L2:42 | TOC:77]", kb)
				}
			}
		})
	}
}

// После «рестарта» (новый Bot, данные не из памяти) T: рендерит из БД.
func TestHandleCallbackQuery_DigestViewFromDBAfterRestart(t *testing.T) {
	ctx := context.Background()
	st, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	digestID, err := st.InsertDigest(ctx, 555, 1, "STORED BODY", 1)
	if err != nil {
		t.Fatalf("InsertDigest: %v", err)
	}
	if err := st.MarkDelivered(ctx, digestID, []storage.DigestItem{
		{DedupKey: "k1", Title: "Item One"},
	}); err != nil {
		t.Fatalf("MarkDelivered: %v", err)
	}

	// «Рестарт»: новый Bot; viewer читает ТОЛЬКО из SQLite (без digest-рендера
	// — импорт digest здесь невозможен из-за цикла; достаточно raw body).
	rec := &recorder{}
	b := &Bot{chatID: 111, log: discardLogger(), ackFn: rec.ack, editFn: rec.edit,
		digestViewer: func(ctx context.Context, req DigestViewRequest) (DigestView, bool, error) {
			if req.Kind != ViewKindTopic {
				return DigestView{}, false, nil
			}
			d, ok, err := st.GetDigest(ctx, req.ID)
			if err != nil || !ok {
				return DigestView{}, ok, err
			}
			return DigestView{Text: d.Body}, true, nil
		}}
	b.handleCallbackQuery(ctx, cbQuery("T:"+strconv.FormatInt(digestID, 10), 111, true))

	if len(rec.texts) != 1 || rec.texts[0] != "STORED BODY" {
		t.Fatalf("edits = %v, want [STORED BODY] from the DB", rec.texts)
	}
	// Несуществующий digestID → not found (crafted id не роняет viewer).
	rec2 := &recorder{}
	b.ackFn, b.editFn = rec2.ack, rec2.edit
	b.handleCallbackQuery(ctx, cbQuery("T:999999", 111, true))
	if len(rec2.texts) != 1 || rec2.texts[0] != digestViewNotFoundText {
		t.Errorf("unknown id edits = %v, want [%q]", rec2.texts, digestViewNotFoundText)
	}
	// Легаси run_id=0: TOC:0 → пусто → not found.
	rec3 := &recorder{}
	b.ackFn, b.editFn = rec3.ack, rec3.edit
	b.handleCallbackQuery(ctx, cbQuery("TOC:0", 111, true))
	if len(rec3.texts) != 1 || rec3.texts[0] != digestViewNotFoundText {
		t.Errorf("TOC:0 edits = %v, want [%q]", rec3.texts, digestViewNotFoundText)
	}
}
