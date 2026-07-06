// Package config loads and validates the bot configuration.
//
// Secrets (bot token, LLM API key, MTProto credentials) come from environment
// variables; non-secret settings may come from an optional YAML file. See
// docs/TECHNICAL_PLAN.md §2. BYOK: an empty LLM_API_KEY activates the offline
// summarizer.
//
// Stage 0: skeleton only — real loading/validation lands in Этап 1+.
package config

import "os"

// Config holds all runtime settings for the bot service.
type Config struct {
	// Telegram delivery / commands (Bot API).
	TelegramBotToken string
	TelegramChatID   string // the single user's chat; digests are delivered here

	// LLM (BYOK). Empty APIKey => offline extractive summarizer.
	LLMAPIKey  string
	LLMBaseURL string
	LLMModel   string

	// State.
	DBPath string // SQLite file, e.g. /data/state.db

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
		DBPath:           envOr("DB_PATH", "/data/state.db"),
		DigestTime:       envOr("DIGEST_TIME", "09:00"),
		Timezone:         envOr("TZ", "UTC"),
	}
	cfg.Offline = cfg.LLMAPIKey == ""
	return cfg, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
