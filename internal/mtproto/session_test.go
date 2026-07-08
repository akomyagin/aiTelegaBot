package mtproto

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func newKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	return key
}

func TestRoundtripEncrypted(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "s.session")
	st, err := NewEncryptedFileStorage(path, newKey(t))
	if err != nil {
		t.Fatalf("new storage: %v", err)
	}

	want := []byte(`{"dc":2,"addr":"example"}`)
	if err := st.StoreSession(ctx, want); err != nil {
		t.Fatalf("store: %v", err)
	}

	// File on disk must not equal plaintext (it is encrypted) and be 0600.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if bytes.Equal(raw, want) {
		t.Fatal("session file is not encrypted on disk")
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatalf("stat: %v", err)
	} else if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("perm = %o, want 600", perm)
	}

	got, err := st.LoadSession(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("roundtrip mismatch: got %q want %q", got, want)
	}
}

func TestRoundtripPlaintext(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "s.session")
	st, err := NewEncryptedFileStorage(path, nil)
	if err != nil {
		t.Fatalf("new storage: %v", err)
	}

	want := []byte("plain-session-bytes")
	if err := st.StoreSession(ctx, want); err != nil {
		t.Fatalf("store: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if !bytes.Equal(raw, want) {
		t.Fatal("plaintext mode should store data verbatim")
	}
	got, err := st.LoadSession(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("roundtrip mismatch: got %q want %q", got, want)
	}
}

func TestTamperDetected(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "s.session")
	st, err := NewEncryptedFileStorage(path, newKey(t))
	if err != nil {
		t.Fatalf("new storage: %v", err)
	}
	if err := st.StoreSession(ctx, []byte("secret")); err != nil {
		t.Fatalf("store: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Flip a byte in the ciphertext (past the 12-byte nonce).
	raw[len(raw)-1] ^= 0xFF
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write tampered: %v", err)
	}

	if _, err := st.LoadSession(ctx); err == nil {
		t.Fatal("expected error loading tampered session, got nil")
	}
}

func TestLoadMissingFile(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "does-not-exist.session")
	st, err := NewEncryptedFileStorage(path, newKey(t))
	if err != nil {
		t.Fatalf("new storage: %v", err)
	}
	got, err := st.LoadSession(ctx)
	if err != nil {
		t.Fatalf("missing file should not error, got %v", err)
	}
	if got != nil {
		t.Fatalf("missing file should return nil, got %q", got)
	}
}

func TestBadKeyLength(t *testing.T) {
	if _, err := NewEncryptedFileStorage("x", []byte("short")); err == nil {
		t.Fatal("expected error for non-32-byte key")
	}
}
