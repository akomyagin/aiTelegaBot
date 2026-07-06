// Command bot is the service entrypoint for aiTelegaBot: a personal AI digest
// Telegram bot (collects RSS/arXiv/HN + Telegram channels, summarizes via LLM,
// delivers a scheduled digest).
//
// main is intentionally thin: it builds a cancellable context wired to SIGINT/
// SIGTERM (graceful shutdown of the long-running service, per
// docs/TECHNICAL_PLAN.md §3.1), loads config, and hands off to internal/app.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/akomyagin/aiTelegaBot/internal/app"
	"github.com/akomyagin/aiTelegaBot/internal/config"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		slog.Error("config load failed", "err", err)
		os.Exit(1)
	}

	if err := app.Run(ctx, cfg); err != nil {
		slog.Error("bot exited with error", "err", err)
		os.Exit(1)
	}
}
