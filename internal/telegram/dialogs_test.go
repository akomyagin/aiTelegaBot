package telegram

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"

	"github.com/akomyagin/aiTelegaBot/internal/mtproto"
)

// updateGoldens regenerates testdata/*.golden on `go test ./internal/telegram
// -update` (plan §6: golden tests with the -update flag).
var updateGoldens = flag.Bool("update", false, "update golden files under testdata/")

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func goldenCheck(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateGoldens {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run go test with -update to create)", path, err)
	}
	if string(want) != got {
		t.Errorf("golden %s mismatch:\n--- want ---\n%s\n--- got ---\n%s", path, want, got)
	}
}

// samplePeers builds n deterministic channel peers; even-indexed ones have a
// username, odd-indexed ones are private.
func samplePeers(n int) []mtproto.PeerInfo {
	peers := make([]mtproto.PeerInfo, 0, n)
	for i := 0; i < n; i++ {
		p := mtproto.PeerInfo{
			MTProtoID: int64(1000 + i),
			BotAPIID:  -1000000000000 - int64(1000+i),
			Title:     fmt.Sprintf("Channel %d", i+1),
			Kind:      "channel",
		}
		if i%2 == 0 {
			p.Username = fmt.Sprintf("chan%d", i+1)
		}
		peers = append(peers, p)
	}
	return peers
}

func mixedPeers() []mtproto.PeerInfo {
	return []mtproto.PeerInfo{
		{MTProtoID: 1, BotAPIID: -1000000000001, Title: "News <&>", Username: "news&<bot>", Kind: "channel"},
		{MTProtoID: 2, BotAPIID: -1000000000002, Title: "Secret group", Kind: "supergroup"},
		{MTProtoID: 3, Title: "Old basic chat", Kind: "chat"},
		{MTProtoID: 4, Title: "Alice", Username: "alice", Kind: "user"},
		{MTProtoID: 5, BotAPIID: -1000000000005, Username: "noname", Kind: "channel"},
		{MTProtoID: 6, BotAPIID: -1000000000006, Kind: "channel"},
	}
}

func kbDump(kb *models.InlineKeyboardMarkup) string {
	var sb strings.Builder
	if kb == nil {
		return "(no keyboard)\n"
	}
	for _, row := range kb.InlineKeyboard {
		for _, btn := range row {
			fmt.Fprintf(&sb, "[%s -> %s]\n", btn.Text, btn.CallbackData)
		}
	}
	return sb.String()
}

func TestRenderDialogsPage_Golden(t *testing.T) {
	tests := []struct {
		name   string
		peers  []mtproto.PeerInfo
		offset int
	}{
		{"dialogs_empty", nil, 0},
		{"dialogs_one", samplePeers(1), 0},
		{"dialogs_eight_p1", samplePeers(8), 0},
		{"dialogs_nine_p1", samplePeers(9), 0},
		{"dialogs_nine_p2", samplePeers(9), 8},
		{"dialogs_mixed", mixedPeers(), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, kb := renderDialogsPage(tt.peers, tt.offset)
			goldenCheck(t, tt.name+".golden", text)
			goldenCheck(t, tt.name+"_kb.golden", kbDump(kb))
		})
	}
}

func TestRenderDialogsPage_MixedKinds(t *testing.T) {
	_, kb := renderDialogsPage(mixedPeers(), 0)
	if kb == nil {
		t.Fatal("keyboard = nil, want add buttons for channel/supergroup")
	}
	// 3 addable peers (channel, supergroup, channel with only a username,
	// channel without title) minus non-addable chat/user: rows = adds + nav.
	var adds, navs int
	for _, row := range kb.InlineKeyboard {
		for _, btn := range row {
			if strings.HasPrefix(btn.CallbackData, "ad:") {
				adds++
			}
			if strings.HasPrefix(btn.CallbackData, "dp:") {
				navs++
			}
		}
	}
	if adds != 4 {
		t.Errorf("add buttons = %d, want 4 (channel+supergroup only)", adds)
	}
	if navs != 0 {
		t.Errorf("nav buttons = %d, want 0 (single page)", navs)
	}
}

func TestRenderDialogsPage_Navigation(t *testing.T) {
	peers := samplePeers(9)
	_, kb1 := renderDialogsPage(peers, 0)
	if kb1 == nil {
		t.Fatal("page 1 keyboard = nil")
	}
	last := kb1.InlineKeyboard[len(kb1.InlineKeyboard)-1]
	if len(last) != 1 || last[0].Text != "▶" || last[0].CallbackData != "dp:8" {
		t.Errorf("page 1 nav row = %+v, want single ▶ dp:8", last)
	}
	_, kb2 := renderDialogsPage(peers, 8)
	last2 := kb2.InlineKeyboard[len(kb2.InlineKeyboard)-1]
	found := false
	for _, btn := range last2 {
		if btn.Text == "◀" && btn.CallbackData == "dp:0" {
			found = true
		}
	}
	if !found {
		t.Errorf("page 2 nav row = %+v, want ◀ dp:0", last2)
	}
}

// newDialogsTestBot builds a Bot without a library client (api == nil):
// dialogs come from the injected snapshot, callbacks go through the ack/edit
// seams. checkForwardAdmin is a no-op with a nil api, so BotIsAdmin is false.
func newDialogsTestBot(peers []mtproto.PeerInfo, registrar ForwardRegistrar) (*Bot, *[]string, *[]string) {
	var acks, edits []string
	b := &Bot{
		chatID:           123,
		log:              discardLogger(),
		forwardRegistrar: registrar,
		dialogLister: func(context.Context, int) ([]mtproto.PeerInfo, error) {
			return peers, nil
		},
		ackFn: func(_ context.Context, _, text string) error {
			acks = append(acks, text)
			return nil
		},
		editFn: func(_ context.Context, _ int64, _ int, text string, _ *models.InlineKeyboardMarkup) error {
			edits = append(edits, text)
			return nil
		},
	}
	return b, &acks, &edits
}

func callbackUpdate(fromID int64, data string, accessible bool) *models.Update {
	mim := models.MaybeInaccessibleMessage{}
	if accessible {
		mim.Message = &models.Message{Chat: models.Chat{ID: 123}, ID: 7}
	} else {
		mim.InaccessibleMessage = &models.InaccessibleMessage{}
	}
	return &models.Update{
		CallbackQuery: &models.CallbackQuery{
			ID:      "cb1",
			From:    models.User{ID: fromID},
			Message: mim,
			Data:    data,
		},
	}
}

func TestHandleCallback_DialogAddOwner(t *testing.T) {
	peers := []mtproto.PeerInfo{
		{MTProtoID: 1234567890, BotAPIID: -1001234567890, Title: "Private <Chan>", Kind: "supergroup"},
	}
	var got ForwardOrigin
	var called bool
	registrar := func(_ context.Context, o ForwardOrigin) string {
		called = true
		got = o
		return "✅ Источник #5 добавлен: Private &lt;Chan&gt; (приватный канал через MTProto)."
	}
	b, acks, edits := newDialogsTestBot(peers, registrar)

	b.handleUpdate(context.Background(), nil, callbackUpdate(123, "ad:-1001234567890", true))

	if !called {
		t.Fatal("registrar was not called for the owner's ad: callback")
	}
	want := ForwardOrigin{BotAPIChatID: -1001234567890, Title: "Private <Chan>"}
	if got != want {
		t.Errorf("registrar origin = %+v, want %+v", got, want)
	}
	if len(*acks) != 1 {
		t.Errorf("acks = %v, want exactly one ack", *acks)
	}
	if len(*edits) != 1 || !strings.Contains((*edits)[0], "#5") {
		t.Errorf("edits = %v, want one edit with the registration reply", *edits)
	}
}

func TestHandleCallback_NotOwner(t *testing.T) {
	var called bool
	registrar := func(_ context.Context, _ ForwardOrigin) string { called = true; return "nope" }
	b, acks, edits := newDialogsTestBot(samplePeers(2), registrar)

	b.handleUpdate(context.Background(), nil, callbackUpdate(456, "ad:-1000000001000", true))

	if called {
		t.Error("registrar called for a stranger's callback")
	}
	if len(*edits) != 0 {
		t.Errorf("edits = %v, want none for a stranger", *edits)
	}
	if len(*acks) != 1 || (*acks)[0] != "Не ваш бот" {
		t.Errorf("acks = %v, want [Не ваш бот]", *acks)
	}
}

func TestHandleCallback_UserDialogRefused(t *testing.T) {
	// user/chat peers have BotAPIID == 0: a crafted "ad:0" finds the first
	// such peer and must be refused by the kind gate before registration.
	peers := []mtproto.PeerInfo{
		{MTProtoID: 777, Title: "Alice", Kind: "user"},
		{MTProtoID: 888, Title: "Old group", Kind: "chat"},
	}
	var called bool
	registrar := func(_ context.Context, _ ForwardOrigin) string { called = true; return "nope" }
	b, acks, edits := newDialogsTestBot(peers, registrar)

	b.handleUpdate(context.Background(), nil, callbackUpdate(123, "ad:0", true))

	if called {
		t.Error("registrar called for a user dialog")
	}
	if len(*acks) != 1 {
		t.Errorf("acks = %v, want exactly one ack", *acks)
	}
	if len(*edits) != 1 || (*edits)[0] != dialogAddForbiddenText {
		t.Errorf("edits = %v, want [%s]", *edits, dialogAddForbiddenText)
	}
}

func TestHandleCallback_InaccessibleMessageAckOnly(t *testing.T) {
	registrar := func(_ context.Context, _ ForwardOrigin) string { return "nope" }
	b, acks, edits := newDialogsTestBot(samplePeers(2), registrar)

	b.handleUpdate(context.Background(), nil, callbackUpdate(123, "ad:-1000000001000", false))

	if len(*acks) != 1 {
		t.Errorf("acks = %v, want exactly one ack", *acks)
	}
	if len(*edits) != 0 {
		t.Errorf("edits = %v, want none for an inaccessible message", *edits)
	}
}

func TestHandleCallback_DialogPage(t *testing.T) {
	peers := samplePeers(9)
	registrar := func(_ context.Context, _ ForwardOrigin) string { return "nope" }
	b, _, edits := newDialogsTestBot(peers, registrar)

	b.handleUpdate(context.Background(), nil, callbackUpdate(123, "dp:8", true))

	want, _ := renderDialogsPage(peers, 8)
	if len(*edits) != 1 || (*edits)[0] != want {
		t.Errorf("edits = %v, want [%s]", *edits, want)
	}
}

// TestRenderDialogsPage_ButtonByteLimit guards the Bot API 64-BYTE inline
// button label limit (BUTTON_TEXT_INVALID, review H1): a 30-rune Cyrillic
// title (60 bytes, 78 with the «Добавить: » prefix) and a multi-byte emoji
// title must both render buttons within the limit.
func TestRenderDialogsPage_ButtonByteLimit(t *testing.T) {
	tests := []struct {
		name  string
		peers []mtproto.PeerInfo
	}{
		{"cyrillic30", []mtproto.PeerInfo{
			{MTProtoID: 1, BotAPIID: -1000000000001, Title: strings.Repeat("абв", 10), Kind: "channel"},
		}},
		{"emoji", []mtproto.PeerInfo{
			{MTProtoID: 2, BotAPIID: -1000000000002, Title: strings.Repeat("🎉🚀", 12), Kind: "channel"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, kb := renderDialogsPage(tt.peers, 0)
			if kb == nil {
				t.Fatal("keyboard = nil, want an add button")
			}
			for _, row := range kb.InlineKeyboard {
				for _, btn := range row {
					if n := len([]byte(btn.Text)); n > 64 {
						t.Errorf("button text = %d bytes (> 64): %q", n, btn.Text)
					}
				}
			}
		})
	}
}

// TestHandleCallback_GarbageData covers crafted/garbage callback payloads:
// bad ids, bad page numbers, page clamping and alignment, and an unknown
// prefix with no Stage-8-D handler installed (review L3/L4).
func TestHandleCallback_GarbageData(t *testing.T) {
	peers := samplePeers(9)
	page1, _ := renderDialogsPage(peers, 0)
	page2, _ := renderDialogsPage(peers, 8)

	tests := []struct {
		name      string
		data      string
		wantEdits []string // exact edit texts; nil => no edit expected
		wantAck   string   // expected single ack text
		checkKB   func(t *testing.T, kb *models.InlineKeyboardMarkup)
	}{
		{
			name:      "ad non-numeric",
			data:      "ad:abc",
			wantEdits: []string{dialogBadIDText},
		},
		{
			name:      "ad empty rest",
			data:      "ad:",
			wantEdits: []string{dialogBadIDText},
		},
		{
			name:      "dp non-numeric",
			data:      "dp:abc",
			wantEdits: []string{dialogBadPageText},
		},
		{
			name:      "dp negative",
			data:      "dp:-1",
			wantEdits: []string{dialogBadPageText},
		},
		{
			name:      "dp overflow clamps to last page",
			data:      "dp:999",
			wantEdits: []string{page2},
		},
		{
			name:      "dp unaligned realigns to page start",
			data:      "dp:3",
			wantEdits: []string{page1},
			checkKB: func(t *testing.T, kb *models.InlineKeyboardMarkup) {
				if kb == nil {
					t.Fatal("keyboard = nil on the aligned first page")
				}
				for _, row := range kb.InlineKeyboard {
					for _, btn := range row {
						if btn.Text == "◀" {
							t.Errorf("offset-0 page has a ◀ button: %+v", btn)
						}
					}
				}
			},
		},
		{
			name:      "unknown prefix acks empty",
			data:      "zz:1",
			wantEdits: nil,
			wantAck:   "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var acks, edits []string
			var lastKB *models.InlineKeyboardMarkup
			b := &Bot{
				chatID: 123,
				log:    discardLogger(),
				dialogLister: func(context.Context, int) ([]mtproto.PeerInfo, error) {
					return peers, nil
				},
				ackFn: func(_ context.Context, _, text string) error {
					acks = append(acks, text)
					return nil
				},
				editFn: func(_ context.Context, _ int64, _ int, text string, kb *models.InlineKeyboardMarkup) error {
					edits = append(edits, text)
					lastKB = kb
					return nil
				},
			}

			b.handleUpdate(context.Background(), nil, callbackUpdate(123, tt.data, true))

			if len(edits) != len(tt.wantEdits) {
				t.Fatalf("edits = %v, want %v", edits, tt.wantEdits)
			}
			for i := range tt.wantEdits {
				if edits[i] != tt.wantEdits[i] {
					t.Errorf("edit[%d] = %q, want %q", i, edits[i], tt.wantEdits[i])
				}
			}
			if len(acks) != 1 || acks[0] != tt.wantAck {
				t.Errorf("acks = %v, want exactly one %q", acks, tt.wantAck)
			}
			if tt.checkKB != nil {
				tt.checkKB(t, lastKB)
			}
		})
	}
}

func TestDialogsSnapshot_RateLimit(t *testing.T) {
	calls := 0
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	now := base
	b := &Bot{
		chatID: 123,
		log:    discardLogger(),
		nowFn:  func() time.Time { return now },
		dialogLister: func(context.Context, int) ([]mtproto.PeerInfo, error) {
			calls++
			return samplePeers(3), nil
		},
	}
	ctx := context.Background()

	// Three calls inside the 30s window: the lister runs exactly once and the
	// repeats reuse the snapshot (plan §4.6).
	for i := 0; i < 3; i++ {
		if _, err := b.cachedDialogs(ctx); err != nil {
			t.Fatalf("dialogsSnapshot: %v", err)
		}
		now = now.Add(10 * time.Second)
	}
	if calls != 1 {
		t.Errorf("GetDialogs calls = %d, want 1 within the 30s window", calls)
	}

	// Past the window: a real fetch again.
	now = base.Add(31 * time.Second)
	if _, err := b.cachedDialogs(ctx); err != nil {
		t.Fatalf("dialogsSnapshot: %v", err)
	}
	if calls != 2 {
		t.Errorf("GetDialogs calls = %d, want 2 after the 30s window", calls)
	}
}

func TestHandleDialogs_NoMTProto(t *testing.T) {
	var replies []string
	b := &Bot{
		chatID: 123,
		log:    discardLogger(),
		deliverFn: func(_ context.Context, _, text string) error {
			replies = append(replies, text)
			return nil
		},
	}

	u := &models.Update{Message: &models.Message{
		Chat: models.Chat{ID: 123},
		Text: "/dialogs",
	}}
	b.handleUpdate(context.Background(), nil, u)

	if len(replies) != 1 || replies[0] != dialogsUnavailableText {
		t.Errorf("replies = %v, want [%s]", replies, dialogsUnavailableText)
	}
}

func TestHandleDialogs_FirstPage(t *testing.T) {
	peers := samplePeers(9)
	var sentText string
	var sentKB *models.InlineKeyboardMarkup
	b := &Bot{
		chatID: 123,
		log:    discardLogger(),
		dialogLister: func(context.Context, int) ([]mtproto.PeerInfo, error) {
			return peers, nil
		},
		sendFn: func(_ context.Context, chatID int64, text string, kb *models.InlineKeyboardMarkup) error {
			if chatID != 123 {
				t.Errorf("send chatID = %d, want 123", chatID)
			}
			sentText, sentKB = text, kb
			return nil
		},
	}

	u := &models.Update{Message: &models.Message{
		Chat: models.Chat{ID: 123},
		Text: "/dialogs",
	}}
	b.handleUpdate(context.Background(), nil, u)

	wantText, wantKB := renderDialogsPage(peers, 0)
	if sentText != wantText {
		t.Errorf("sent text = %q, want %q", sentText, wantText)
	}
	if kbDump(sentKB) != kbDump(wantKB) {
		t.Errorf("sent keyboard = %s, want %s", kbDump(sentKB), kbDump(wantKB))
	}
}
