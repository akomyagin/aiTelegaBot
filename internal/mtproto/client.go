package mtproto

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
)

// MTProtoConfig holds the credentials and session settings for the client.
type MTProtoConfig struct {
	AppID       int
	AppHash     string
	SessionPath string
	SessionKey  []byte // 32-byte AES-256 key; empty => plaintext session
}

// Client wraps a gotd/td telegram.Client for one-shot interactive login.
// Channel reading (Этап 7) is intentionally out of scope here.
type Client struct {
	cfg     MTProtoConfig
	storage *EncryptedFileStorage
	client  *telegram.Client
}

// NewClient builds a Client. It fails only on an obviously invalid session key
// length; credential validation happens when the client actually connects.
func NewClient(cfg MTProtoConfig) (*Client, error) {
	if len(cfg.SessionKey) == 0 {
		slog.Warn("MTProto session stored unencrypted — set MTPROTO_SESSION_KEY for production")
	}
	storage, err := NewEncryptedFileStorage(cfg.SessionPath, cfg.SessionKey)
	if err != nil {
		return nil, err
	}
	client := telegram.NewClient(cfg.AppID, cfg.AppHash, telegram.Options{
		SessionStorage: &gotdStorage{inner: storage},
	})
	return &Client{cfg: cfg, storage: storage, client: client}, nil
}

// gotdStorage adapts EncryptedFileStorage to gotd's expectation that a missing
// session is reported as session.ErrNotFound (our store returns (nil, nil) so
// the plain roundtrip tests stay simple).
type gotdStorage struct {
	inner *EncryptedFileStorage
}

func (g *gotdStorage) LoadSession(ctx context.Context) ([]byte, error) {
	data, err := g.inner.LoadSession(ctx)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, session.ErrNotFound
	}
	return data, nil
}

func (g *gotdStorage) StoreSession(ctx context.Context, data []byte) error {
	return g.inner.StoreSession(ctx, data)
}

// IsAuthorized reports whether the stored session is a logged-in account.
func (c *Client) IsAuthorized(ctx context.Context) (bool, error) {
	var authorized bool
	err := c.client.Run(ctx, func(ctx context.Context) error {
		status, err := c.client.Auth().Status(ctx)
		if err != nil {
			return err
		}
		authorized = status.Authorized
		return nil
	})
	if err != nil {
		return false, err
	}
	return authorized, nil
}

// Login performs an interactive authorization: it prompts for phone, the code,
// and (if 2FA is enabled) the password on stdin, then persists the session.
func (c *Client) Login(ctx context.Context) error {
	return c.client.Run(ctx, func(ctx context.Context) error {
		flow := auth.NewFlow(
			stdinAuth{},
			auth.SendCodeOptions{},
		)
		if err := c.client.Auth().IfNecessary(ctx, flow); err != nil {
			return fmt.Errorf("mtproto: auth flow: %w", err)
		}
		self, err := c.client.Self(ctx)
		if err != nil {
			return fmt.Errorf("mtproto: fetch self: %w", err)
		}
		slog.Info("MTProto login successful", "user_id", self.ID, "username", self.Username)
		return nil
	})
}

// Login is a convenience entrypoint used by the `bot login` subcommand: it
// builds a client from cfg and runs the interactive flow.
func Login(ctx context.Context, cfg MTProtoConfig) error {
	if cfg.AppID == 0 || cfg.AppHash == "" {
		return fmt.Errorf("mtproto: MTPROTO_APP_ID and MTPROTO_APP_HASH must be set for login")
	}
	c, err := NewClient(cfg)
	if err != nil {
		return err
	}
	return c.Login(ctx)
}

// stdinAuth implements auth.UserAuthenticator by prompting on stdin. Sign-up of
// new accounts is not supported (this is a login for an existing account).
type stdinAuth struct{}

func (stdinAuth) SignUp(_ context.Context) (auth.UserInfo, error) {
	return auth.UserInfo{}, fmt.Errorf("mtproto: sign-up not supported; use an existing account")
}

func (stdinAuth) AcceptTermsOfService(_ context.Context, _ tg.HelpTermsOfService) error {
	return fmt.Errorf("mtproto: sign-up not supported")
}

func (stdinAuth) Phone(_ context.Context) (string, error) {
	return prompt("Phone (international format, e.g. +79991234567): ")
}

func (stdinAuth) Password(_ context.Context) (string, error) {
	return prompt("2FA password: ")
}

func (stdinAuth) Code(_ context.Context, _ *tg.AuthSentCode) (string, error) {
	return prompt("Confirmation code: ")
}

func prompt(label string) (string, error) {
	fmt.Fprint(os.Stderr, label)
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}
