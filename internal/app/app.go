// Package app is the single composition root: it builds all dependencies
// (store, telegram bot, LLM summarizer, sources, scheduler) from config and
// runs the long-lived service until ctx is cancelled (graceful shutdown).
//
// Stage 0: wires the skeleton together so the service starts and stops cleanly.
// Real behavior fills in per stage (see docs/TECHNICAL_PLAN.md §9).
package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/akomyagin/aiTelegaBot/internal/config"
	"github.com/akomyagin/aiTelegaBot/internal/digest"
	"github.com/akomyagin/aiTelegaBot/internal/feed"
	"github.com/akomyagin/aiTelegaBot/internal/llm"
	"github.com/akomyagin/aiTelegaBot/internal/mtproto"
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
		summarizer = llm.NewClient(cfg.LLMAPIKey, cfg.LLMBaseURL, cfg.LLMModel, cfg.LLMMaxRetries)
	}

	// Web sources from config (Этап 2). Telegram sources land in Этап 5.
	var sources []feed.Source
	hc := &http.Client{Timeout: 30 * time.Second}
	for _, u := range cfg.FeedURLs {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		kind := "rss"
		if strings.Contains(u, "arxiv.org") {
			kind = "arxiv"
		}
		sources = append(sources, feed.NewRSSSource(u, u, kind, hc))
	}
	if cfg.HNLimit > 0 {
		sources = append(sources, feed.NewHNSource("Hacker News", cfg.HNLimit, hc))
	}

	// Telegram sources (Этап 5).
	var channelBuf *telegram.ChannelBuffer
	if len(cfg.TGManagedChannels) > 0 {
		channelBuf = &telegram.ChannelBuffer{}
		for _, ch := range cfg.TGManagedChannels {
			ch = strings.TrimPrefix(strings.TrimSpace(ch), "@")
			if ch == "" {
				continue
			}
			sources = append(sources, telegram.NewManagedSource("@"+ch, channelBuf))
		}
	}
	tgLimit := cfg.TGSourceLimit
	if tgLimit <= 0 {
		tgLimit = 20
	}
	for _, ch := range cfg.TGPublicChannels {
		ch = strings.TrimPrefix(strings.TrimSpace(ch), "@")
		if ch == "" {
			continue
		}
		sources = append(sources, telegram.NewPublicSource("@"+ch, ch, hc, tgLimit))
	}

	// MTProto private-channel sources (Этап 7). Only wired when credentials and
	// at least one channel are configured; otherwise no MTProto client is built.
	if cfg.MTProtoAppID != 0 && cfg.MTProtoAppHash != "" && len(cfg.MTProtoChannels) > 0 {
		mtClient, err := mtproto.NewClient(mtproto.MTProtoConfig{
			AppID:       cfg.MTProtoAppID,
			AppHash:     cfg.MTProtoAppHash,
			SessionPath: cfg.MTProtoSession,
			SessionKey:  cfg.MTProtoKey,
		})
		if err != nil {
			return err
		}
		mtLimit := cfg.MTProtoLimit
		if mtLimit <= 0 {
			mtLimit = 20
		}
		for _, ch := range cfg.MTProtoChannels {
			ch = strings.TrimPrefix(strings.TrimSpace(ch), "@")
			if ch == "" {
				continue
			}
			sources = append(sources, mtproto.NewChannelSource("@"+ch, ch, mtLimit, mtClient))
		}
	}

	pipeline := &digest.Pipeline{
		Sources:   sources,
		Store:     store,
		Summarize: summarizer,
		ChatID:    cfg.TelegramChatID,
	}

	listSources := func(_ context.Context) (string, error) {
		if len(sources) == 0 {
			return "Источники не настроены. Укажите FEED_URLS и/или HN_LIMIT.", nil
		}
		var b strings.Builder
		b.WriteString("Активные источники:\n")
		for i, src := range sources {
			fmt.Fprintf(&b, "%d. %s\n", i+1, src.Name())
		}
		return b.String(), nil
	}

	botOpts := []telegram.Option{
		telegram.WithDigestTrigger(pipeline.Run),
		telegram.WithSourceLister(listSources),
	}
	if channelBuf != nil {
		botOpts = append(botOpts, telegram.WithChannelBuffer(channelBuf))
	}
	bot, err := telegram.NewBot(cfg.TelegramBotToken, cfg.TelegramChatID, botOpts...)
	if err != nil {
		return err
	}
	pipeline.Deliver = bot

	// Bridge SQLite meta to the scheduler for slot idempotency across restarts.
	getSlot := func(ctx context.Context) (string, error) {
		v, _, err := store.GetMeta(ctx, "last_digest_date")
		return v, err
	}
	setSlot := func(ctx context.Context, date string) error {
		return store.SetMeta(ctx, "last_digest_date", date)
	}

	sched, err := scheduler.New(
		cfg.DigestTime, cfg.Timezone, pipeline.Run,
		scheduler.WithSlotStore(getSlot, setSlot),
	)
	if err != nil {
		return err
	}

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
