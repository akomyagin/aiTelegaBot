// Тесты Этапа 9E ЧАСТЬ 1 (план §4.5.2, тест-кейсы 3/6 — digest-сторона):
// ветка «P:<itemID>» диспетчера — CallbackResult{Send} c media_ref (file_id и
// URL), caption (EscapeHTML + лимит 1024 байт, golden compareGolden/-update),
// кнопки «✖ Закрыть» PDEL (callback_data ≤64 байт), media_ref='' / item miss →
// error seam'а (ack «Не получилось открыть» на стороне telegram), truncateToBytes.

package digest

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/akomyagin/aiTelegaBot/internal/storage"
	"github.com/akomyagin/aiTelegaBot/internal/telegram"
)

func TestDrillPhotoSend(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name        string
		item        storage.DigestItem
		wantMedia   string
		wantCaption string // golden-имя (пусто → сравнение не выполняем)
	}{
		{name: "file_id + ссылка", item: storage.DigestItem{
			ID: 11, Title: "Заголовок <1> & «2»", Summary: "Саммари топика",
			URL: "https://a.example/1?x=1&y=2", MediaKind: "photo", MediaRef: "AgADBAAD",
		}, wantMedia: "AgADBAAD", wantCaption: "photo_caption_basic"},
		{name: "URL media_ref", item: storage.DigestItem{
			ID: 12, Title: "Пост", Summary: "Сводка",
			URL: "https://a.example/2", MediaKind: "photo", MediaRef: "https://cdn.example/p.jpg",
		}, wantMedia: "https://cdn.example/p.jpg", wantCaption: "photo_caption_url"},
		{name: "без ссылки → deeplink t.me", item: storage.DigestItem{
			ID: 13, Title: "TG пост", Summary: "Сводка",
			TgPeer: "durov", TgMsgID: 5, MediaKind: "photo", MediaRef: "AgAD2",
		}, wantMedia: "AgAD2", wantCaption: "photo_caption_deeplink"},
		{name: "нет ни URL ни peer → без строки «↪»", item: storage.DigestItem{
			ID: 14, Title: "", Summary: "", MediaKind: "photo", MediaRef: "AgAD3",
		}, wantMedia: "AgAD3", wantCaption: "photo_caption_nolink"},
		{name: "переполнение: кириллица+эмодзи → ≤1024 байт", item: storage.DigestItem{
			ID: 15, Title: strings.Repeat("Ящер", 200), Summary: strings.Repeat("🖼 саммари ", 150),
			URL: "https://a.example/" + strings.Repeat("q", 1200), MediaKind: "photo", MediaRef: "AgAD4",
		}, wantMedia: "AgAD4", wantCaption: "photo_caption_long"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &drillStore{hasItem: true, item: tt.item}
			dd := NewDrillDown(store, nil)
			res, err := dd(ctx, "P:"+strconv.FormatInt(tt.item.ID, 10))
			if err != nil || res == nil || res.Send == nil {
				t.Fatalf("P:%d = %+v, %v, want Send", tt.item.ID, res, err)
			}
			if res.Edit != nil {
				t.Error("Edit должен быть nil (sendPhoto, не edit)")
			}
			if res.Send.PhotoMediaRef != tt.wantMedia {
				t.Errorf("PhotoMediaRef = %q, want %q", res.Send.PhotoMediaRef, tt.wantMedia)
			}
			// Кнопки: одна строка, «✖ Закрыть» PDEL; callback_data ≤64 байт.
			want := [][]telegram.InlineButton{{{Text: "✖ Закрыть", Data: "PDEL"}}}
			assertButtons(t, "P: photo buttons", res.Send.PhotoButtons, want)
			if len("PDEL") > 64 || len("✖ Закрыть") > 64 {
				t.Error("бюджет ≤64 байт нарушен")
			}
			cap := res.Send.PhotoCaption
			if len(cap) > photoCaptionMaxBytes {
				t.Errorf("caption = %d байт > %d", len(cap), photoCaptionMaxBytes)
			}
			if !utf8.ValidString(cap) {
				t.Error("caption обрезан не на границе рун")
			}
			if tt.wantCaption != "" {
				compareGolden(t, tt.wantCaption, cap)
			}
		})
	}

	// media_ref пуст → error seam'а (telegram ack'ит «Не получилось открыть»;
	// НЕ «Медиа недоступно» — 9D-заглушка удалена).
	for _, ref := range []string{"", "   "} {
		store := &drillStore{hasItem: true, item: storage.DigestItem{ID: 21, Title: "T", MediaRef: ref}}
		dd := NewDrillDown(store, nil)
		if res, err := dd(ctx, "P:21"); err == nil || res != nil {
			t.Errorf("P:21 media_ref=%q = %+v, %v, want error/nil", ref, res, err)
		}
	}
	// item не найден / мусор → error, sendPhoto не готовится.
	if res, err := NewDrillDown(&drillStore{hasItem: false}, nil)(ctx, "P:777"); err == nil || res != nil {
		t.Errorf("P:777 = %+v, %v, want error/nil", res, err)
	}
	if res, err := NewDrillDown(nil, nil)(ctx, "P:garbage"); err == nil || res != nil {
		t.Errorf("P:garbage = %+v, %v, want error/nil", res, err)
	}
}

// truncateToBytes — обобщение TruncateButtonLabelTo: ≤maxBytes БАЙТ на границе
// рун, «…» при обрезке; короткий вход не меняется.
func TestTruncateToBytes(t *testing.T) {
	short := "<b>ок</b>"
	if got := truncateToBytes(short, 1024); got != short {
		t.Errorf("короткий = %q, want без изменений", got)
	}
	long := strings.Repeat("фыв", 500) // 3000 байт
	got := truncateToBytes(long, 1024)
	if len(got) > 1024 {
		t.Errorf("len = %d > 1024", len(got))
	}
	if !strings.HasSuffix(got, "…") || !utf8.ValidString(got) {
		t.Errorf("резка не на границе рун: %q…", got[:20])
	}
	// Ровно на границе — без «…».
	exact := strings.Repeat("a", 1024)
	if got := truncateToBytes(exact, 1024); got != exact {
		t.Errorf("exact 1024 изменился: %d байт", len(got))
	}
}
