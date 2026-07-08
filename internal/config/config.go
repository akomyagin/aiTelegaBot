// Package config loads and validates the bot configuration.
//
// Secrets (bot token, LLM API key, MTProto credentials) come from environment
// variables; non-secret settings may come from an optional YAML file. See
// docs/TECHNICAL_PLAN.md §2. BYOK: an empty LLM_API_KEY activates the offline
// summarizer.
//
// Stage 0: skeleton only — real loading/validation lands in Этап 1+.
package config

import (
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds all runtime settings for the bot service.
type Config struct {
	// Telegram delivery / commands (Bot API).
	TelegramBotToken string
	TelegramChatID   string // the single user's chat; digests are delivered here

	// LLM (BYOK). Empty APIKey => offline extractive summarizer.
	LLMAPIKey     string
	LLMBaseURL    string
	LLMModel      string
	LLMMaxRetries int // env LLM_MAX_RETRIES, default 3

	// State.
	DBPath string // SQLite file, e.g. /data/state.db

	// Web sources (Этап 2).
	FeedURLs []string // RSS/Atom/arXiv feed URLs (env FEED_URLS, comma-separated)
	HNLimit  int      // number of Hacker News top stories (env HN_LIMIT, default 15)

	// Telegram sources (Этап 5).
	TGPublicChannels  []string // env TG_PUBLIC_CHANNELS, comma-separated
	TGManagedChannels []string // env TG_MANAGED_CHANNELS, comma-separated (chat usernames)
	TGSourceLimit     int      // env TG_SOURCE_LIMIT, default 20 — max posts per channel

	// MTProto (Фаза 2, Этап 6).
	MTProtoAppID   int    // env MTPROTO_APP_ID
	MTProtoAppHash string // env MTPROTO_APP_HASH
	MTProtoSession string // env MTPROTO_SESSION_PATH, default "/data/session.encrypted"
	MTProtoKey     []byte // env MTPROTO_SESSION_KEY (hex, 32 bytes); empty = unencrypted

	// Scheduling.
	DigestTime string // daily slot, e.g. "09:00"
	Timezone   string // IANA TZ, e.g. "Europe/Moscow"

	// Offline is true when no LLM key is configured.
	Offline bool
}

// Load reads configuration from the environment (and, later, an optional YAML
// file), validates it, and returns a Config.
//
// Stage 0: returns a Config populated from env with sensible defaults; full
// validation is implemented in Этап 1.
func Load() (*Config, error) {
	cfg := &Config{
		TelegramBotToken: os.Getenv("TELEGRAM_BOT_TOKEN"),
		TelegramChatID:   os.Getenv("TELEGRAM_CHAT_ID"),
		LLMAPIKey:        os.Getenv("LLM_API_KEY"),
		LLMBaseURL:       os.Getenv("LLM_BASE_URL"),
		LLMModel:         os.Getenv("LLM_MODEL"),
		LLMMaxRetries:    parseIntOr(os.Getenv("LLM_MAX_RETRIES"), 3),
		DBPath:           envOr("DB_PATH", "/data/state.db"),
		DigestTime:       envOr("DIGEST_TIME", "09:00"),
		Timezone:         envOr("TZ", "UTC"),
		FeedURLs:         parseCSV(os.Getenv("FEED_URLS")),
		HNLimit:          parseIntOr(os.Getenv("HN_LIMIT"), 15),

		TGPublicChannels:  parseCSV(os.Getenv("TG_PUBLIC_CHANNELS")),
		TGManagedChannels: parseCSV(os.Getenv("TG_MANAGED_CHANNELS")),
		TGSourceLimit:     parseIntOr(os.Getenv("TG_SOURCE_LIMIT"), 20),

		MTProtoAppID:   parseIntOr(os.Getenv("MTPROTO_APP_ID"), 0),
		MTProtoAppHash: os.Getenv("MTPROTO_APP_HASH"),
		MTProtoSession: envOr("MTPROTO_SESSION_PATH", "/data/session.encrypted"),
	}
	cfg.Offline = cfg.LLMAPIKey == ""

	key, err := parseHexKey(os.Getenv("MTPROTO_SESSION_KEY"))
	if err != nil {
		return nil, err
	}
	cfg.MTProtoKey = key

	return cfg, nil
}

// parseHexKey decodes a hex-encoded 32-byte session key. An empty value is
// valid (unencrypted session); a non-empty value must decode to exactly 32
// bytes.
func parseHexKey(raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	key, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("config: MTPROTO_SESSION_KEY is not valid hex: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("config: MTPROTO_SESSION_KEY must decode to 32 bytes, got %d", len(key))
	}
	return key, nil
}

// parseCSV splits a comma-separated list, trimming whitespace and dropping
// empty entries.
func parseCSV(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if u := strings.TrimSpace(part); u != "" {
			out = append(out, u)
		}
	}
	return out
}

// parseIntOr returns the parsed int or def on any parse error.
func parseIntOr(raw string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
		return n
	}
	return def
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
