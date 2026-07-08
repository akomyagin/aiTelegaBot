// Package mtproto implements the MTProto (gotd/td) side of aiTelegaBot: a
// one-shot interactive login and an encrypted session store. Реализация — Этап 6.
//
// A Telegram session grants full account access, so it is treated as the most
// sensitive secret: the session file lives outside git (see .gitignore), is
// written with 0600 permissions, and is encrypted at rest with AES-GCM when a
// key is configured (env MTPROTO_SESSION_KEY, hex-encoded 32 bytes).
package mtproto

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
)

// EncryptedFileStorage persists a gotd/td session to a single file.
//
// It satisfies gotd's telegram.SessionStorage interface (LoadSession /
// StoreSession). When key is non-empty the session is encrypted with AES-GCM
// (a fresh random 12-byte nonce is prepended to the ciphertext); when key is
// nil/empty the session is stored as plaintext (dev convenience — a warning is
// logged by the caller).
type EncryptedFileStorage struct {
	path string
	key  []byte // nil/empty => plaintext
}

// NewEncryptedFileStorage returns a session store writing to path. A non-empty
// key must be exactly 32 bytes (AES-256); an empty key selects plaintext mode.
func NewEncryptedFileStorage(path string, key []byte) (*EncryptedFileStorage, error) {
	if len(key) != 0 && len(key) != 32 {
		return nil, fmt.Errorf("mtproto: session key must be 32 bytes, got %d", len(key))
	}
	return &EncryptedFileStorage{path: path, key: key}, nil
}

// LoadSession reads and (if a key is set) decrypts the session file.
//
// A missing file returns (nil, nil): an empty session is the normal state on
// the very first run. Callers that need gotd's ErrNotFound semantics wrap this
// store (see client.go).
func (s *EncryptedFileStorage) LoadSession(_ context.Context) ([]byte, error) {
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("mtproto: read session: %w", err)
	}
	if len(s.key) == 0 {
		return raw, nil
	}

	gcm, err := s.newGCM()
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(raw) < ns {
		return nil, errors.New("mtproto: session file too short / corrupt")
	}
	nonce, ciphertext := raw[:ns], raw[ns:]
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("mtproto: decrypt session (wrong key or tampered file): %w", err)
	}
	return plain, nil
}

// StoreSession writes the session, encrypting with AES-GCM when a key is set.
// The file is created/overwritten with 0600 permissions.
func (s *EncryptedFileStorage) StoreSession(_ context.Context, data []byte) error {
	out := data
	if len(s.key) != 0 {
		gcm, err := s.newGCM()
		if err != nil {
			return err
		}
		nonce := make([]byte, gcm.NonceSize())
		if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
			return fmt.Errorf("mtproto: nonce: %w", err)
		}
		// Prepend nonce to ciphertext.
		out = gcm.Seal(nonce, nonce, data, nil)
	}
	if err := os.WriteFile(s.path, out, 0o600); err != nil {
		return fmt.Errorf("mtproto: write session: %w", err)
	}
	return nil
}

func (s *EncryptedFileStorage) newGCM() (cipher.AEAD, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, fmt.Errorf("mtproto: aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("mtproto: gcm: %w", err)
	}
	return gcm, nil
}
