package telegram

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/go-telegram/bot/models"
)

// newForwardTestBot builds a Bot without a library client (api == nil):
// replies go through the deliverFn seam, forwards through the recording
// registrar. checkForwardAdmin is a no-op with a nil api, so BotIsAdmin is
// always false here.
func newForwardTestBot(t *testing.T, registrar ForwardRegistrar) (*Bot, *[]string) {
	t.Helper()
	var replies []string
	b := &Bot{
		chatID:           123,
		log:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		forwardRegistrar: registrar,
		deliverFn: func(_ context.Context, _, text string) error {
			replies = append(replies, text)
			return nil
		},
	}
	return b, &replies
}

func channelForwardUpdate(chatID, originChatID int64, username, title string) *models.Update {
	return &models.Update{
		Message: &models.Message{
			Chat: models.Chat{ID: chatID},
			ForwardOrigin: &models.MessageOrigin{
				Type: models.MessageOriginTypeChannel,
				MessageOriginChannel: &models.MessageOriginChannel{
					Chat: models.Chat{ID: originChatID, Username: username, Title: title},
				},
			},
		},
	}
}

func TestHandleUpdate_ForwardFromOwner(t *testing.T) {
	var called bool
	var got ForwardOrigin
	registrar := func(_ context.Context, o ForwardOrigin) string {
		called = true
		got = o
		return "registered"
	}
	b, replies := newForwardTestBot(t, registrar)

	u := channelForwardUpdate(123, -1001234567890, "chan", "My Channel")
	b.handleUpdate(context.Background(), nil, u)

	if !called {
		t.Fatal("registrar was not called for an owner forward")
	}
	want := ForwardOrigin{
		BotAPIChatID: -1001234567890,
		Username:     "chan",
		Title:        "My Channel",
		BotIsAdmin:   false, // nil api => admin check skipped
	}
	if got != want {
		t.Errorf("registrar origin = %+v, want %+v", got, want)
	}
	if len(*replies) != 1 || (*replies)[0] != "registered" {
		t.Errorf("replies = %v, want [registered]", *replies)
	}
}

func TestHandleUpdate_ForwardPrivateNoUsername(t *testing.T) {
	var got ForwardOrigin
	registrar := func(_ context.Context, o ForwardOrigin) string {
		got = o
		return "ok"
	}
	b, _ := newForwardTestBot(t, registrar)

	// MessageOriginChat (private supergroup forward), no username.
	u := &models.Update{
		Message: &models.Message{
			Chat: models.Chat{ID: 123},
			ForwardOrigin: &models.MessageOrigin{
				Type: models.MessageOriginTypeChat,
				MessageOriginChat: &models.MessageOriginChat{
					SenderChat: models.Chat{ID: -1009876543210, Title: "Secret <Chat>"},
				},
			},
		},
	}
	b.handleUpdate(context.Background(), nil, u)

	if got.BotAPIChatID != -1009876543210 || got.Username != "" || got.Title != "Secret <Chat>" {
		t.Errorf("origin = %+v, want private chat origin", got)
	}
}

func TestHandleUpdate_ForwardFromStrangerIgnored(t *testing.T) {
	var called bool
	registrar := func(_ context.Context, _ ForwardOrigin) string {
		called = true
		return "nope"
	}
	b, replies := newForwardTestBot(t, registrar)

	u := channelForwardUpdate(456, -1001234567890, "chan", "My Channel")
	b.handleUpdate(context.Background(), nil, u)

	if called {
		t.Error("registrar called for a forward from an unauthorized chat")
	}
	if len(*replies) != 0 {
		t.Errorf("replies = %v, want none for a stranger", *replies)
	}
}

func TestHandleUpdate_ForwardHiddenUserRefused(t *testing.T) {
	var called bool
	registrar := func(_ context.Context, _ ForwardOrigin) string {
		called = true
		return "nope"
	}
	b, replies := newForwardTestBot(t, registrar)

	u := &models.Update{
		Message: &models.Message{
			Chat: models.Chat{ID: 123},
			ForwardOrigin: &models.MessageOrigin{
				Type:                    models.MessageOriginTypeHiddenUser,
				MessageOriginHiddenUser: &models.MessageOriginHiddenUser{SenderUserName: "someone"},
			},
		},
	}
	b.handleUpdate(context.Background(), nil, u)

	if called {
		t.Error("registrar called for a hidden-user forward")
	}
	if len(*replies) != 1 || (*replies)[0] != forwardUnsupportedText {
		t.Errorf("replies = %v, want [%s]", *replies, forwardUnsupportedText)
	}
}

func TestHandleUpdate_NonForwardStillRoutesCommands(t *testing.T) {
	var called bool
	registrar := func(_ context.Context, _ ForwardOrigin) string {
		called = true
		return "nope"
	}
	b, replies := newForwardTestBot(t, registrar)

	u := &models.Update{
		Message: &models.Message{Chat: models.Chat{ID: 123}, Text: "/start"},
	}
	b.handleUpdate(context.Background(), nil, u)

	if called {
		t.Error("registrar called for a non-forward message")
	}
	if len(*replies) != 1 || (*replies)[0] != startText {
		t.Errorf("replies = %v, want [startText]", *replies)
	}
}

func TestParseCommand(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		wantCmd string
		wantOK  bool
	}{
		{"plain", "/start", "start", true},
		{"botname suffix", "/help@MyBot", "help", true},
		{"with arg", "/digest arg", "digest", true},
		{"uppercase", "/SOURCES", "sources", true},
		{"addsource", "/addsource https://x", "addsource", true},
		{"removesource", "/removesource 5", "removesource", true},
		{"not a command", "hello", "", false},
		{"empty", "", "", false},
		{"bare slash", "/", "", false},
		{"leading space", "  /start", "start", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, ok := parseCommand(tt.text)
			if cmd != tt.wantCmd || ok != tt.wantOK {
				t.Fatalf("parseCommand(%q) = (%q, %v), want (%q, %v)",
					tt.text, cmd, ok, tt.wantCmd, tt.wantOK)
			}
		})
	}
}

func TestAuthorized(t *testing.T) {
	tests := []struct {
		name     string
		incoming int64
		owner    int64
		want     bool
	}{
		{"match", 123, 123, true},
		{"mismatch", 456, 123, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := authorized(tt.incoming, tt.owner); got != tt.want {
				t.Fatalf("authorized(%d, %d) = %v, want %v",
					tt.incoming, tt.owner, got, tt.want)
			}
		})
	}
}
