// Package app is the single composition root: it builds all dependencies
// (store, telegram bot, LLM summarizer, sources, scheduler) from config and
// runs the long-lived service until ctx is cancelled (graceful shutdown).
//
// Stage 0: wires the skeleton together so the service starts and stops cleanly.
// Real behavior fills in per stage (see docs/TECHNICAL_PLAN.md §9).
package app

import (
	"context"
	"log/slog"

	"github.com/akomyagin/aiTelegaBot/internal/config"
	"github.com/akomyagin/aiTelegaBot/internal/digest"
	"github.com/akomyagin/aiTelegaBot/internal/llm"
	"github.com/akomyagin/aiTelegaBot/internal/scheduler"
	"github.com/akomyagin/aiTelegaBot/internal/storage"
	"github.com/akomyagin/aiTelegaBot/internal/telegram"
)

// Run builds dependencies and runs the service until ctx is cancelled.
func Run(ctx context.Context, cfg *config.Config) error {
	store, err := storage.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	// LLM: BYOK — offline summarizer when no key is configured.
	var summarizer llm.Summarizer
	if cfg.Offline {
		slog.Info("no LLM API key configured; using offline summarizer")
		summarizer = llm.NewOffline()
	} else {
		summarizer = llm.NewClient(cfg.LLMAPIKey, cfg.LLMBaseURL, cfg.LLMModel)
	}

	pipeline := &digest.Pipeline{
		Store:     store,
		Summarize: summarizer,
		ChatID:    cfg.TelegramChatID,
		// Sources are registered in Этап 2 (web) / Этап 5 (Telegram).
	}

	// listSources is a placeholder until real sources land in Этап 2.
	listSources := func(ctx context.Context) (string, error) {
		return "Источники будут добавлены в Этапе 2", nil
	}

	bot, err := telegram.NewBot(
		cfg.TelegramBotToken,
		cfg.TelegramChatID,
		telegram.WithDigestTrigger(pipeline.Run),
		telegram.WithSourceLister(listSources),
	)
	if err != nil {
		return err
	}
	pipeline.Deliver = bot

	sched := scheduler.New(cfg.DigestTime, cfg.Timezone, pipeline.Run)

	// Stage 0: start the long-poll bot and the scheduler; both stop on ctx.Done.
	slog.Info("aiTelegaBot starting", "offline", cfg.Offline, "db", cfg.DBPath)

	errCh := make(chan error, 2)
	go func() { errCh <- bot.Run(ctx) }()
	go func() { errCh <- sched.Run(ctx) }()

	<-ctx.Done()
	slog.Info("shutdown signal received; stopping")
	// Drain both goroutines; ctx cancellation makes them return.
	<-errCh
	<-errCh
	return nil
}
