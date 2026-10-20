package telegram

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/go-telegram/bot/models"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
)

// Stage 8 B (темы): тесты telegram-стороны — golden-рендер /topics и
// tl-пикера, /newtopic (экранирование, лимит 40 рун), parse-ошибки
// /settopic, callback'и ts/tl/tn от owner и не-owner, suggested-поток после
// добавления (и деградация без него), байтовый лимит лейблов кнопок.
// Фейки — замыкания; сеть и БД не нужны. Хелперы goldenCheck/kbDump/
// callbackUpdate/discardLogger — из dialogs_test.go.

// cmdUpdate builds an owner/stranger text-message update for handleUpdate.
func cmdUpdate(chatID int64, text string) *models.Update {
	return &models.Update{
		Message: &models.Message{Chat: models.Chat{ID: chatID}, Text: text},
	}
}

// sentMsg captures one send/edit with its keyboard.
type sentMsg struct {
	text string
	kb   *models.InlineKeyboardMarkup
}

// topicRecorder records every outgoing transport call of a test bot.
type topicRecorder struct {
	replies []string  // deliverFn (текст без клавиатуры)
	sends   []sentMsg // sendFn (текст + клавиатура)
	acks    []string  // ackFn
	edits   []sentMsg // editFn
}

// newTopicsTestBot builds a Bot without a library client (api == nil) and
// records all four transports. Seams (topicLister etc.) are set per test.
func newTopicsTestBot() (*Bot, *topicRecorder) {
	rec := &topicRecorder{}
	b := &Bot{
		chatID: 123,
		log:    discardLogger(),
		deliverFn: func(_ context.Context, _, text string) error {
			rec.replies = append(rec.replies, text)
			return nil
		},
		sendFn: func(_ context.Context, _ int64, text string, kb *models.InlineKeyboardMarkup) error {
			rec.sends = append(rec.sends, sentMsg{text, kb})
			return nil
		},
		ackFn: func(_ context.Context, _, text string) error {
			rec.acks = append(rec.acks, text)
			return nil
		},
		editFn: func(_ context.Context, _ int64, _ int, text string, kb *models.InlineKeyboardMarkup) error {
			rec.edits = append(rec.edits, sentMsg{text, kb})
			return nil
		},
	}
	return b, rec
}

func sampleTopicsView() []TopicView {
	return []TopicView{
		{ID: 1, Name: "Без темы", Sources: []TopicSourceView{
			{ID: 3, Kind: "rss", Ref: "https://example.com/feed", Enabled: true},
			{ID: 5, Kind: "tg_public", Ref: "chan<&>", Enabled: false},
		}},
		{ID: 2, Name: "AI <3", Sources: nil},
	}
}

// pickerTopics builds n deterministic topics (id 1..n, «Тема i») for the
// tl-pagination tests.
func pickerTopics(n int) []TopicView {
	topics := make([]TopicView, 0, n)
	for i := 1; i <= n; i++ {
		topics = append(topics, TopicView{ID: int64(i), Name: fmt.Sprintf("Тема %d", i)})
	}
	return topics
}

func TestRenderTopicsList_Golden(t *testing.T) {
	goldenCheck(t, "topics_list.golden", renderTopicsList(sampleTopicsView()))
	goldenCheck(t, "topics_empty.golden", renderTopicsList(nil))
}

func TestRenderTopicPickerPage_Golden(t *testing.T) {
	topics := pickerTopics(9)
	text1, kb1 := renderTopicPickerPage(topics, 5, 0)
	goldenCheck(t, "topics_p1.golden", text1)
	goldenCheck(t, "topics_p1_kb.golden", kbDump(kb1))
	text2, kb2 := renderTopicPickerPage(topics, 5, 8)
	goldenCheck(t, "topics_p2.golden", text2)
	goldenCheck(t, "topics_p2_kb.golden", kbDump(kb2))
}

// TestRenderTopicPickerPage_Navigation: 9 тем → 2 страницы, «◀ / ▶» с
// пагинацией внутри префикса tl: (tl:<sourceID>:<offset>), НЕ dp:.
func TestRenderTopicPickerPage_Navigation(t *testing.T) {
	topics := pickerTopics(9)

	_, kb1 := renderTopicPickerPage(topics, 5, 0)
	if kb1 == nil {
		t.Fatal("page 1 keyboard = nil")
	}
	last := kb1.InlineKeyboard[len(kb1.InlineKeyboard)-1]
	if len(last) != 1 || last[0].Text != "▶" || last[0].CallbackData != "tl:5:8" {
		t.Errorf("page 1 nav row = %+v, want single ▶ tl:5:8", last)
	}
	for _, row := range kb1.InlineKeyboard {
		for _, btn := range row {
			if strings.HasPrefix(btn.CallbackData, "dp:") {
				t.Errorf("topic picker must not use dp: (dialogs prefix): %q", btn.CallbackData)
			}
		}
	}

	_, kb2 := renderTopicPickerPage(topics, 5, 8)
	found := false
	for _, btn := range kb2.InlineKeyboard[len(kb2.InlineKeyboard)-1] {
		if btn.Text == "◀" && btn.CallbackData == "tl:5:0" {
			found = true
		}
	}
	if !found {
		t.Errorf("page 2 nav = %+v, want ◀ tl:5:0", kb2.InlineKeyboard[len(kb2.InlineKeyboard)-1])
	}
	if n := len(kb2.InlineKeyboard); n != 2 { // 1 тема + nav-ряд
		t.Errorf("page 2 rows = %d, want 2 (1 topic + nav)", n)
	}
}

// TestRenderTopicPickerPage_ButtonByteLimit guards the Bot API 64-BYTE button
// label limit (урок A2 H1): 40-символьное имя темы в кириллице (80 байт) и
// эмодзи-имя должны укладываться в лимит с «…».
func TestRenderTopicPickerPage_ButtonByteLimit(t *testing.T) {
	tests := []struct {
		name  string
		topic string
	}{
		{"cyrillic40", strings.Repeat("аб", 20)},             // 40 рун, 80 байт
		{"emoji", strings.Repeat("🎉🚀", 12)},                  // 24 рун, 96 байт
		{"exactly61", strings.Repeat("x", topicLabelBudget)}, // без усечения
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, kb := renderTopicPickerPage([]TopicView{{ID: 7, Name: tt.topic}}, 5, 0)
			if kb == nil {
				t.Fatal("keyboard = nil, want a ts: button")
			}
			btn := kb.InlineKeyboard[0][0]
			if n := len([]byte(btn.Text)); n > 64 {
				t.Errorf("button text = %d bytes (> 64): %q", n, btn.Text)
			}
			if btn.CallbackData != "ts:5:7" {
				t.Errorf("callback data = %q, want ts:5:7", btn.CallbackData)
			}
		})
	}
}

func TestHandleTopics(t *testing.T) {
	t.Run("renders via seam", func(t *testing.T) {
		b, rec := newTopicsTestBot()
		b.topicLister = func(context.Context) ([]TopicView, error) { return sampleTopicsView(), nil }
		b.handleUpdate(context.Background(), nil, cmdUpdate(123, "/topics"))
		want := renderTopicsList(sampleTopicsView())
		if len(rec.replies) != 1 || rec.replies[0] != want {
			t.Errorf("replies = %v, want [%s]", rec.replies, want)
		}
	})
	t.Run("nil seam degrades", func(t *testing.T) {
		b, rec := newTopicsTestBot()
		b.handleUpdate(context.Background(), nil, cmdUpdate(123, "/topics"))
		if len(rec.replies) != 1 || rec.replies[0] != topicsUnavailableText {
			t.Errorf("replies = %v, want [%s]", rec.replies, topicsUnavailableText)
		}
	})
	t.Run("seam error", func(t *testing.T) {
		b, rec := newTopicsTestBot()
		b.topicLister = func(context.Context) ([]TopicView, error) {
			return nil, fmt.Errorf("db down")
		}
		b.handleUpdate(context.Background(), nil, cmdUpdate(123, "/topics"))
		if len(rec.replies) != 1 || rec.replies[0] != topicsErrorText {
			t.Errorf("replies = %v, want [%s]", rec.replies, topicsErrorText)
		}
	})
}

func TestHandleNewTopic(t *testing.T) {
	t.Run("escapes name, created", func(t *testing.T) {
		b, rec := newTopicsTestBot()
		var gotName string
		b.topicCreator = func(_ context.Context, name string) (NewTopicResult, error) {
			gotName = name
			return NewTopicResult{ID: 2, Created: true}, nil
		}
		b.handleUpdate(context.Background(), nil, cmdUpdate(123, "/newtopic <b>зло</b>"))
		if gotName != "<b>зло</b>" {
			t.Errorf("creator name = %q, want raw <b>зло</b>", gotName)
		}
		if len(rec.replies) != 1 ||
			!strings.Contains(rec.replies[0], "&lt;b&gt;зло&lt;/b&gt;") ||
			!strings.Contains(rec.replies[0], "#2") {
			t.Errorf("replies = %v, want escaped created-topic reply", rec.replies)
		}
	})
	t.Run("already existed", func(t *testing.T) {
		b, rec := newTopicsTestBot()
		b.topicCreator = func(context.Context, string) (NewTopicResult, error) {
			return NewTopicResult{ID: 2, Created: false}, nil
		}
		b.handleUpdate(context.Background(), nil, cmdUpdate(123, "/newtopic AI"))
		if len(rec.replies) != 1 || !strings.Contains(rec.replies[0], "уже существует (#2)") {
			t.Errorf("replies = %v, want «уже существует (#2)»", rec.replies)
		}
	})
	t.Run("over 40 runes refused before seam", func(t *testing.T) {
		b, rec := newTopicsTestBot()
		called := false
		b.topicCreator = func(context.Context, string) (NewTopicResult, error) {
			called = true
			return NewTopicResult{}, nil
		}
		b.handleUpdate(context.Background(), nil,
			cmdUpdate(123, "/newtopic "+strings.Repeat("а", maxTopicNameRunes+1)))
		if called {
			t.Error("creator called for a 41-rune name")
		}
		if len(rec.replies) != 1 || rec.replies[0] != topicNameTooLongText {
			t.Errorf("replies = %v, want [%s]", rec.replies, topicNameTooLongText)
		}
	})
	t.Run("exactly 40 runes accepted", func(t *testing.T) {
		b, rec := newTopicsTestBot()
		called := false
		b.topicCreator = func(context.Context, string) (NewTopicResult, error) {
			called = true
			return NewTopicResult{ID: 3, Created: true}, nil
		}
		b.handleUpdate(context.Background(), nil,
			cmdUpdate(123, "/newtopic "+strings.Repeat("а", maxTopicNameRunes)))
		if !called {
			t.Error("creator not called for a 40-rune name")
		}
		_ = rec
	})
	t.Run("no arg → usage", func(t *testing.T) {
		b, rec := newTopicsTestBot()
		b.handleUpdate(context.Background(), nil, cmdUpdate(123, "/newtopic"))
		if len(rec.replies) != 1 || rec.replies[0] != topicCreateUsageText {
			t.Errorf("replies = %v, want [%s]", rec.replies, topicCreateUsageText)
		}
	})
}

func TestHandleSetTopic_ParseErrors(t *testing.T) {
	b, rec := newTopicsTestBot()
	called := false
	b.topicBinder = func(context.Context, int64, string) (TopicBindResult, error) {
		called = true
		return TopicBindResult{}, nil
	}
	tests := []struct {
		name string
		text string
		want string
	}{
		{"no args", "/settopic", setTopicUsageText},
		{"missing topic arg", "/settopic 5", setTopicUsageText},
		{"garbage source id", "/settopic abc 1", setTopicBadSourceIDText},
		{"spaces only after id", "/settopic 5    ", setTopicUsageText},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec.replies = nil
			b.handleUpdate(context.Background(), nil, cmdUpdate(123, tt.text))
			if called {
				t.Error("binder called on a parse error")
			}
			if len(rec.replies) != 1 || rec.replies[0] != tt.want {
				t.Errorf("replies = %v, want [%s]", rec.replies, tt.want)
			}
		})
	}
}

func TestHandleSetTopic_Binds(t *testing.T) {
	t.Run("numeric and name forms", func(t *testing.T) {
		b, rec := newTopicsTestBot()
		var gotID int64
		var gotArg string
		b.topicBinder = func(_ context.Context, sourceID int64, topicArg string) (TopicBindResult, error) {
			gotID, gotArg = sourceID, topicArg
			return TopicBindResult{SourceFound: true, TopicFound: true, TopicID: 3, TopicName: "AI <3>"}, nil
		}
		b.handleUpdate(context.Background(), nil, cmdUpdate(123, "/settopic 5 3"))
		if gotID != 5 || gotArg != "3" {
			t.Errorf("binder got (%d, %q), want (5, \"3\")", gotID, gotArg)
		}
		want := "✅ Источник #5 → тема AI &lt;3&gt;."
		if len(rec.replies) != 1 || rec.replies[0] != want {
			t.Errorf("replies = %v, want [%s]", rec.replies, want)
		}

		rec.replies = nil
		b.handleUpdate(context.Background(), nil, cmdUpdate(123, "/settopic 5 AI <3>"))
		if gotID != 5 || gotArg != "AI <3>" {
			t.Errorf("binder got (%d, %q), want (5, \"AI <3>\") — имя с пробелами передаётся как есть", gotID, gotArg)
		}
	})
	t.Run("source not found", func(t *testing.T) {
		b, rec := newTopicsTestBot()
		b.topicBinder = func(context.Context, int64, string) (TopicBindResult, error) {
			return TopicBindResult{SourceFound: false}, nil
		}
		b.handleUpdate(context.Background(), nil, cmdUpdate(123, "/settopic 99 1"))
		if len(rec.replies) != 1 || rec.replies[0] != "Источника с id 99 нет. /sources — список." {
			t.Errorf("replies = %v, want source-not-found", rec.replies)
		}
	})
	t.Run("topic not found", func(t *testing.T) {
		b, rec := newTopicsTestBot()
		b.topicBinder = func(context.Context, int64, string) (TopicBindResult, error) {
			return TopicBindResult{SourceFound: true, TopicFound: false}, nil
		}
		b.handleUpdate(context.Background(), nil, cmdUpdate(123, "/settopic 5 Нет такой"))
		if len(rec.replies) != 1 || rec.replies[0] != "Темы «Нет такой» нет. /topics — список." {
			t.Errorf("replies = %v, want topic-not-found", rec.replies)
		}
	})
}

func TestHandleCallback_TopicSet(t *testing.T) {
	t.Run("owner binds and edits", func(t *testing.T) {
		b, rec := newTopicsTestBot()
		var gotID int64
		var gotArg string
		b.topicBinder = func(_ context.Context, sourceID int64, topicArg string) (TopicBindResult, error) {
			gotID, gotArg = sourceID, topicArg
			return TopicBindResult{SourceFound: true, TopicFound: true, TopicID: 3, TopicName: "AI <3>"}, nil
		}
		b.handleUpdate(context.Background(), nil, callbackUpdate(123, "ts:5:3", true))
		if gotID != 5 || gotArg != "3" {
			t.Errorf("binder got (%d, %q), want (5, \"3\")", gotID, gotArg)
		}
		if len(rec.acks) != 1 || rec.acks[0] != "" {
			t.Errorf("acks = %v, want exactly one empty ack", rec.acks)
		}
		want := "✅ Источник #5 → тема AI &lt;3&gt;."
		if len(rec.edits) != 1 || rec.edits[0].text != want {
			t.Errorf("edits = %+v, want [%s]", rec.edits, want)
		}
	})
	t.Run("not owner → ack only", func(t *testing.T) {
		b, rec := newTopicsTestBot()
		called := false
		b.topicBinder = func(context.Context, int64, string) (TopicBindResult, error) {
			called = true
			return TopicBindResult{}, nil
		}
		b.handleUpdate(context.Background(), nil, callbackUpdate(456, "ts:5:3", true))
		if called {
			t.Error("binder called for a stranger's callback")
		}
		if len(rec.acks) != 1 || rec.acks[0] != cbNotOwnerText {
			t.Errorf("acks = %v, want [Не ваш бот]", rec.acks)
		}
		if len(rec.edits) != 0 {
			t.Errorf("edits = %+v, want none for a stranger", rec.edits)
		}
	})
	t.Run("inaccessible → ack only", func(t *testing.T) {
		b, rec := newTopicsTestBot()
		called := false
		b.topicBinder = func(context.Context, int64, string) (TopicBindResult, error) {
			called = true
			return TopicBindResult{}, nil
		}
		b.handleUpdate(context.Background(), nil, callbackUpdate(123, "ts:5:3", false))
		if called {
			t.Error("binder called for an inaccessible message")
		}
		if len(rec.acks) != 1 {
			t.Errorf("acks = %v, want exactly one ack", rec.acks)
		}
		if len(rec.edits) != 0 {
			t.Errorf("edits = %+v, want none", rec.edits)
		}
	})
	t.Run("bad data → ack + error edit", func(t *testing.T) {
		b, rec := newTopicsTestBot()
		b.handleUpdate(context.Background(), nil, callbackUpdate(123, "ts:abc", true))
		if len(rec.acks) != 1 {
			t.Errorf("acks = %v, want exactly one ack", rec.acks)
		}
		if len(rec.edits) != 1 || rec.edits[0].text != topicBadCallbackText {
			t.Errorf("edits = %+v, want [%s]", rec.edits, topicBadCallbackText)
		}
	})
}

func TestHandleCallback_TopicNone(t *testing.T) {
	b, rec := newTopicsTestBot()
	b.topicBinder = func(context.Context, int64, string) (TopicBindResult, error) {
		t.Error("binder must not be called for tn:")
		return TopicBindResult{}, nil
	}
	b.handleUpdate(context.Background(), nil, callbackUpdate(123, "tn:5", true))
	if len(rec.acks) != 1 || rec.acks[0] != tnAckText {
		t.Errorf("acks = %v, want [%s]", rec.acks, tnAckText)
	}
	if len(rec.edits) != 0 || len(rec.sends) != 0 || len(rec.replies) != 0 {
		t.Errorf("tn: must not edit/send anything: edits=%+v sends=%+v replies=%v",
			rec.edits, rec.sends, rec.replies)
	}

	// Не-owner: тот же контракт, что у ts: — ack «Не ваш бот», без действий.
	rec.acks = nil
	b.handleUpdate(context.Background(), nil, callbackUpdate(456, "tn:5", true))
	if len(rec.acks) != 1 || rec.acks[0] != cbNotOwnerText {
		t.Errorf("stranger acks = %v, want [Не ваш бот]", rec.acks)
	}
}

func TestHandleCallback_TopicList_Pages(t *testing.T) {
	topics := pickerTopics(9)

	t.Run("first page", func(t *testing.T) {
		b, rec := newTopicsTestBot()
		b.topicLister = func(context.Context) ([]TopicView, error) { return topics, nil }
		b.handleUpdate(context.Background(), nil, callbackUpdate(123, "tl:5", true))
		wantText, wantKB := renderTopicPickerPage(topics, 5, 0)
		if len(rec.edits) != 1 || rec.edits[0].text != wantText {
			t.Fatalf("edits = %+v, want page-1 text", rec.edits)
		}
		if kbDump(rec.edits[0].kb) != kbDump(wantKB) {
			t.Errorf("page-1 kb =\n%s\nwant\n%s", kbDump(rec.edits[0].kb), kbDump(wantKB))
		}
		if len(rec.acks) != 1 {
			t.Errorf("acks = %v, want exactly one ack", rec.acks)
		}
	})
	t.Run("second page via offset", func(t *testing.T) {
		b, rec := newTopicsTestBot()
		b.topicLister = func(context.Context) ([]TopicView, error) { return topics, nil }
		b.handleUpdate(context.Background(), nil, callbackUpdate(123, "tl:5:8", true))
		wantText, _ := renderTopicPickerPage(topics, 5, 8)
		if len(rec.edits) != 1 || rec.edits[0].text != wantText {
			t.Fatalf("edits = %+v, want page-2 text", rec.edits)
		}
	})
	t.Run("bad source id", func(t *testing.T) {
		b, rec := newTopicsTestBot()
		b.handleUpdate(context.Background(), nil, callbackUpdate(123, "tl:abc", true))
		if len(rec.edits) != 1 || rec.edits[0].text != topicBadCallbackText {
			t.Errorf("edits = %+v, want [%s]", rec.edits, topicBadCallbackText)
		}
	})
	t.Run("not owner", func(t *testing.T) {
		b, rec := newTopicsTestBot()
		b.topicLister = func(context.Context) ([]TopicView, error) {
			t.Error("lister called for a stranger's callback")
			return nil, nil
		}
		b.handleUpdate(context.Background(), nil, callbackUpdate(456, "tl:5", true))
		if len(rec.acks) != 1 || rec.acks[0] != cbNotOwnerText || len(rec.edits) != 0 {
			t.Errorf("stranger: acks=%v edits=%+v", rec.acks, rec.edits)
		}
	})
}

func TestHandleAddSource_SuggestionFlow(t *testing.T) {
	const wantPlain = "✅ Источник #5 добавлен (rss: https://x). Свежих постов: 2."

	t.Run("no suggestion option → old text, no buttons", func(t *testing.T) {
		b, rec := newTopicsTestBot()
		b.addSource = func(context.Context, string, string) (SourceAddResult, error) {
			return SourceAddResult{ID: 5, Fresh: 2}, nil
		}
		b.handleUpdate(context.Background(), nil, cmdUpdate(123, "/addsource https://x"))
		if len(rec.replies) != 1 || rec.replies[0] != wantPlain {
			t.Errorf("replies = %v, want [%s]", rec.replies, wantPlain)
		}
		if len(rec.sends) != 0 {
			t.Errorf("sends = %+v, want none without the suggestion seam", rec.sends)
		}
	})
	t.Run("suggestion nil (no LLM key) → old text", func(t *testing.T) {
		b, rec := newTopicsTestBot()
		b.addSource = func(context.Context, string, string) (SourceAddResult, error) {
			return SourceAddResult{ID: 5, Fresh: 2}, nil
		}
		b.topicSuggest = func(context.Context, int64, string, string, []feed.Item) *TopicSuggestion { return nil }
		b.handleUpdate(context.Background(), nil, cmdUpdate(123, "/addsource https://x"))
		if len(rec.replies) != 1 || rec.replies[0] != wantPlain {
			t.Errorf("replies = %v, want [%s]", rec.replies, wantPlain)
		}
		if len(rec.sends) != 0 {
			t.Errorf("sends = %+v, want none for a nil suggestion", rec.sends)
		}
	})
	t.Run("suggestion → text + ts/tl/tn keyboard", func(t *testing.T) {
		b, rec := newTopicsTestBot()
		b.addSource = func(context.Context, string, string) (SourceAddResult, error) {
			return SourceAddResult{ID: 5, Fresh: 2, Sample: []feed.Item{{Title: "trial"}}}, nil
		}
		var gotID int64
		var gotKind, gotRef string
		var gotSample []feed.Item
		b.topicSuggest = func(_ context.Context, sourceID int64, kind, ref string, sample []feed.Item) *TopicSuggestion {
			gotID, gotKind, gotRef, gotSample = sourceID, kind, ref, sample
			return &TopicSuggestion{TopicID: 9, TopicName: "AI <3>"}
		}
		b.handleUpdate(context.Background(), nil, cmdUpdate(123, "/addsource https://x"))
		if gotID != 5 || gotKind != "rss" || gotRef != "https://x" {
			t.Errorf("suggest got (%d, %q, %q), want (5, rss, https://x)", gotID, gotKind, gotRef)
		}
		if len(gotSample) != 1 || gotSample[0].Title != "trial" {
			t.Errorf("suggest sample = %+v, want trial-элемент из пути добавления (M3)", gotSample)
		}
		if len(rec.sends) != 1 {
			t.Fatalf("sends = %+v, want exactly one message with keyboard", rec.sends)
		}
		if !strings.HasPrefix(rec.sends[0].text, wantPlain) ||
			!strings.Contains(rec.sends[0].text, "Предложенная тема: AI &lt;3&gt;") {
			t.Errorf("send text = %q, want base + escaped suggestion", rec.sends[0].text)
		}
		want := "[Привязать -> ts:5:9]\n[Другая тема -> tl:5]\n[Без темы -> tn:5]\n"
		if got := kbDump(rec.sends[0].kb); got != want {
			t.Errorf("kb =\n%swant\n%s", got, want)
		}
		for _, row := range rec.sends[0].kb.InlineKeyboard {
			for _, btn := range row {
				if len([]byte(btn.Text)) > 64 || len([]byte(btn.CallbackData)) > 64 {
					t.Errorf("button over 64 bytes: %q / %q", btn.Text, btn.CallbackData)
				}
			}
		}
	})
}

func TestHandleForward_V2WithSuggestion(t *testing.T) {
	b, rec := newTopicsTestBot()
	b.forwardRegistrarV2 = func(_ context.Context, o ForwardOrigin) ForwardResult {
		return ForwardResult{
			Reply:    "✅ Источник #5 добавлен: My Channel (публичный канал).",
			SourceID: 5, Kind: "tg_public", Ref: "chan",
			Sample: []feed.Item{{Title: "trial"}},
		}
	}
	b.topicSuggest = func(_ context.Context, sourceID int64, kind, ref string, sample []feed.Item) *TopicSuggestion {
		if sourceID != 5 || kind != "tg_public" || ref != "chan" {
			t.Errorf("suggest got (%d, %q, %q)", sourceID, kind, ref)
		}
		if len(sample) != 1 || sample[0].Title != "trial" {
			t.Errorf("suggest sample = %+v, want ForwardResult.Sample (M3)", sample)
		}
		return &TopicSuggestion{TopicID: 9, TopicName: "Новости"}
	}
	u := channelForwardUpdate(123, -1001234567890, "chan", "My Channel")
	b.handleUpdate(context.Background(), nil, u)

	if len(rec.sends) != 1 {
		t.Fatalf("sends = %+v, want one message with keyboard", rec.sends)
	}
	if !strings.Contains(rec.sends[0].text, "Предложенная тема: Новости") {
		t.Errorf("send text = %q, want suggestion line", rec.sends[0].text)
	}
	if got := kbDump(rec.sends[0].kb); !strings.Contains(got, "[Привязать -> ts:5:9]") {
		t.Errorf("kb = %s, want ts:5:9 button", got)
	}
}

func TestHandleForward_LegacyRegistrarUnchanged(t *testing.T) {
	// Без V2 форвард работает как в A1: текст регистратора, без кнопок.
	b, rec := newTopicsTestBot()
	b.forwardRegistrar = func(context.Context, ForwardOrigin) string { return "registered" }
	b.topicSuggest = func(context.Context, int64, string, string, []feed.Item) *TopicSuggestion {
		t.Error("suggestion must not be called without a V2 registrar (no sourceID)")
		return nil
	}
	b.handleUpdate(context.Background(), nil, channelForwardUpdate(123, -1001234567890, "chan", "My Channel"))
	if len(rec.replies) != 1 || rec.replies[0] != "registered" || len(rec.sends) != 0 {
		t.Errorf("replies = %v, sends = %+v, want [registered] and no sends", rec.replies, rec.sends)
	}
}

func TestHandleRemoveSource_StaticAndDynamic(t *testing.T) {
	tests := []struct {
		name   string
		v2     SourceRemoverV2
		legacy func(ctx context.Context, id int64) (bool, error)
		want   string
	}{
		{
			name: "v2 static",
			v2: func(context.Context, int64) (SourceRemoveResult, error) {
				return SourceRemoveResult{Found: true, Static: true}, nil
			},
			want: staticSourceRemoveText,
		},
		{
			name: "v2 dynamic",
			v2:   func(context.Context, int64) (SourceRemoveResult, error) { return SourceRemoveResult{Found: true}, nil },
			want: "✅ Источник #7 отключён.",
		},
		{
			name: "v2 not found",
			v2:   func(context.Context, int64) (SourceRemoveResult, error) { return SourceRemoveResult{}, nil },
			want: "Источника с id 7 нет. /sources — список.",
		},
		{
			name:   "legacy compat",
			legacy: func(context.Context, int64) (bool, error) { return true, nil },
			want:   "✅ Источник #7 отключён.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, rec := newTopicsTestBot()
			b.removeSourceV2 = tt.v2
			b.removeSource = tt.legacy
			b.handleUpdate(context.Background(), nil, cmdUpdate(123, "/removesource 7"))
			if len(rec.replies) != 1 || rec.replies[0] != tt.want {
				t.Errorf("replies = %v, want [%s]", rec.replies, tt.want)
			}
		})
	}
}

// TestHelpTexts_MentionTopicCommands: строки справки ровно по плану
// (623–626): квадратные скобки, без обещания дайджеста по темам (заработает в C).
func TestHelpTexts_MentionTopicCommands(t *testing.T) {
	for _, text := range []string{startText, helpText} {
		for _, want := range []string{
			"/topics — темы и их источники",
			"/newtopic [имя] — новая тема",
			"/settopic [id источника] [id темы] — привязать",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("text missing %q", want)
			}
		}
		if strings.Contains(text, "<") {
			t.Error("help texts must use square brackets, not angle brackets")
		}
	}
}
