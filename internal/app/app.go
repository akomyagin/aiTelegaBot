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

	// Static sources from config. Built once at startup: some carry per-run
	// state (channelBuf, mtClient), so they are reused across runs. Dynamic,
	// DB-backed sources are (re)built on every run — see sourceProvider below.
	var staticSources []feed.Source
	hc := &http.Client{Timeout: 30 * time.Second}
	tgLimit := cfg.TGSourceLimit
	if tgLimit <= 0 {
		tgLimit = 20
	}
	for _, u := range cfg.FeedURLs {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		kind := "rss"
		if strings.Contains(u, "arxiv.org") {
			kind = "arxiv"
		}
		staticSources = append(staticSources, feed.NewRSSSource(u, u, kind, hc))
	}
	if cfg.HNLimit > 0 {
		staticSources = append(staticSources, feed.NewHNSource("Hacker News", cfg.HNLimit, hc))
	}

	// Managed-channel posts flow through a shared buffer filled by handleUpdate.
	// Created unconditionally so /addsource of a managed channel works even when
	// TG_MANAGED_CHANNELS is empty at startup.
	channelBuf := &telegram.ChannelBuffer{}
	for _, ch := range cfg.TGManagedChannels {
		ch = strings.TrimPrefix(strings.TrimSpace(ch), "@")
		if ch == "" {
			continue
		}
		staticSources = append(staticSources, telegram.NewManagedSource("@"+ch, channelBuf))
	}
	for _, ch := range cfg.TGPublicChannels {
		ch = strings.TrimPrefix(strings.TrimSpace(ch), "@")
		if ch == "" {
			continue
		}
		staticSources = append(staticSources, telegram.NewPublicSource("@"+ch, ch, hc, tgLimit))
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
			staticSources = append(staticSources, mtproto.NewChannelSource("@"+ch, ch, mtLimit, mtClient))
		}
	}

	// sourceProvider returns the source set for each run: the static config layer
	// plus a fresh snapshot of enabled DB-backed sources. Called once per Run, so
	// sources added via /addsource are picked up without a restart.
	sourceProvider := func(ctx context.Context) ([]feed.Source, error) {
		dbRows, err := store.ListSources(ctx)
		if err != nil {
			return nil, err
		}
		dyn := buildDynamicSources(dbRows, hc, tgLimit, channelBuf)
		// Static first, then dynamic; order is cosmetic (dedup handles overlap).
		all := make([]feed.Source, 0, len(staticSources)+len(dyn))
		all = append(all, staticSources...)
		all = append(all, dyn...)
		return all, nil
	}

	pipeline := &digest.Pipeline{
		SourceProvider: sourceProvider,
		Store:          store,
		Summarize:      summarizer,
		ChatID:         cfg.TelegramChatID,
	}

	listSources := func(ctx context.Context) (string, error) {
		dbRows, err := store.ListSources(ctx)
		if err != nil {
			return "", err
		}
		var b strings.Builder
		b.WriteString("Статические источники (из .env):\n")
		if len(staticSources) == 0 {
			b.WriteString("  (нет; укажите FEED_URLS и/или HN_LIMIT)\n")
		}
		for _, src := range staticSources {
			fmt.Fprintf(&b, "  • %s\n", src.Name())
		}
		b.WriteString("\nДинамические источники (команды):\n")
		hasDyn := false
		for _, ds := range dbRows {
			status := "on"
			if !ds.Enabled {
				status = "off"
			}
			fmt.Fprintf(&b, "  #%d [%s] %s (%s)\n", ds.ID, ds.Kind, ds.Ref, status)
			hasDyn = true
		}
		if !hasDyn {
			b.WriteString("  (нет; добавьте через /addsource)\n")
		}
		return b.String(), nil
	}

	// addSourceFn validates a candidate source before persisting it, then
	// inserts. Validation differs per kind:
	//   - rss/tg_public: a trial collect MUST hit the network and return ≥1
	//     item, with real errors (not "0 posts") surfaced on failure.
	//   - tg_botapi: Bot API cannot pull channel history, so there is nothing to
	//     trial-collect. Critically, a ManagedSource.Collect call DRAINS the
	//     shared production channelBuf — running it here would silently discard
	//     real posts already buffered for other managed channels. Admin-ness
	//     (checked in detectKind, upstream of this call) is the only available
	//     validation signal.
	addSourceFn := func(ctx context.Context, kind, ref string) (int64, int, error) {
		existing, err := store.ListSources(ctx)
		if err != nil {
			return 0, 0, err
		}
		for _, s := range existing {
			if s.Enabled && s.Kind == kind && s.Ref == ref {
				return 0, 0, fmt.Errorf("источник уже добавлен (#%d)", s.ID)
			}
		}

		if kind == "tg_botapi" {
			id, err := store.AddSource(ctx, kind, ref)
			if err != nil {
				return 0, 0, err
			}
			return id, 0, nil
		}

		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()

		var items []feed.Item
		switch kind {
		case "rss":
			items, err = feed.NewRSSSource(ref, ref, "rss", hc).Collect(cctx)
		case "tg_public":
			items, err = telegram.NewPublicSource("@"+ref, ref, hc, tgLimit).TrialCollect(cctx)
		default:
			return 0, 0, fmt.Errorf("неизвестный вид источника %q", kind)
		}
		if err != nil {
			return 0, 0, fmt.Errorf("источник недоступен: %w", err)
		}
		if len(items) == 0 {
			return 0, 0, fmt.Errorf("источник не вернул ни одного поста (проверьте адрес/канал)")
		}

		id, err := store.AddSource(ctx, kind, ref)
		if err != nil {
			return 0, 0, err
		}
		return id, len(items), nil
	}

	botOpts := []telegram.Option{
		telegram.WithDigestTrigger(pipeline.Run),
		telegram.WithSourceLister(listSources),
		telegram.WithChannelBuffer(channelBuf),
		telegram.WithSourceAdder(addSourceFn),
		telegram.WithSourceRemover(store.DisableSource),
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
