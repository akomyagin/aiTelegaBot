// Package app is the single composition root: it builds all dependencies
// (store, telegram bot, LLM summarizer, sources, scheduler) from config and
// runs the long-lived service until ctx is cancelled (graceful shutdown).
//
// Stage 0: wires the skeleton together so the service starts and stops cleanly.
// Real behavior fills in per stage (see docs/TECHNICAL_PLAN.md §9).
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
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

	// Managed-channel posts flow through a shared per-key buffer filled by
	// handleUpdate (key = username, plan §4.5). Created unconditionally so
	// /addsource of a managed channel works even when TG_MANAGED_CHANNELS is
	// empty at startup.
	channelBuf := &telegram.ChannelBuffer{}
	for _, ch := range cfg.TGManagedChannels {
		ch = strings.TrimPrefix(strings.TrimSpace(ch), "@")
		if ch == "" {
			continue
		}
		staticSources = append(staticSources, telegram.NewManagedSource("@"+ch, ch, channelBuf))
	}
	for _, ch := range cfg.TGPublicChannels {
		ch = strings.TrimPrefix(strings.TrimSpace(ch), "@")
		if ch == "" {
			continue
		}
		staticSources = append(staticSources, telegram.NewPublicSource("@"+ch, ch, hc, tgLimit))
	}

	// MTProto client (Stage 8): built whenever credentials are present,
	// EVEN without MTPROTO_CHANNELS — DB-backed private sources registered via
	// forward / t.me/c links need it. Without credentials the degradation is a
	// first-class path: private-chat registration answers "requires configured
	// MTProto" and dynamic tg_mtproto rows are skipped with a warning.
	var mtClient *mtproto.Client
	peerCache := storagePeerCache{store: store}
	if cfg.MTProtoAppID != 0 && cfg.MTProtoAppHash != "" {
		mc, err := mtproto.NewClient(mtproto.MTProtoConfig{
			AppID:       cfg.MTProtoAppID,
			AppHash:     cfg.MTProtoAppHash,
			SessionPath: cfg.MTProtoSession,
			SessionKey:  cfg.MTProtoKey,
		}, mtproto.WithPeerCache(peerCache))
		if err != nil {
			return err
		}
		mtClient = mc
	} else if len(cfg.MTProtoChannels) > 0 {
		slog.Warn("MTPROTO_CHANNELS is set but MTPROTO_APP_ID/MTPROTO_APP_HASH are missing; private MTProto sources are disabled")
	}
	mtLimit := cfg.MTProtoLimit
	if mtLimit <= 0 {
		mtLimit = 20
	}
	if mtClient != nil {
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
		dyn := buildDynamicSources(dbRows, hc, tgLimit, channelBuf, mtClient, mtLimit)
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
			// Имена и ref'ы могут содержать '<', '>', '&' — ответ уходит с
			// ParseMode HTML (план §4.8, ревью M4).
			fmt.Fprintf(&b, "  • %s\n", telegram.EscapeHTML(src.Name()))
		}
		b.WriteString("\nДинамические источники (команды):\n")
		hasDyn := false
		for _, ds := range dbRows {
			status := "on"
			if !ds.Enabled {
				status = "off"
			}
			fmt.Fprintf(&b, "  #%d [%s] %s (%s)\n", ds.ID, ds.Kind, telegram.EscapeHTML(ds.Ref), status)
			hasDyn = true
		}
		if !hasDyn {
			b.WriteString("  (нет; добавьте через /addsource)\n")
		}
		return b.String(), nil
	}

	addSourceFn := newSourceAdder(store, mtClient, hc, tgLimit)
	registerForward := newForwardRegistrar(store, mtClient, addSourceFn)

	botOpts := []telegram.Option{
		telegram.WithDigestTrigger(pipeline.Run),
		telegram.WithSourceLister(listSources),
		telegram.WithChannelBuffer(channelBuf),
		telegram.WithSourceAdder(addSourceFn),
		telegram.WithSourceRemover(store.DisableSource),
		telegram.WithForwardRegistrar(registerForward),
	}
	// /dialogs (Stage 8 A2): the list comes from the secondary MTProto
	// account; registration of the chosen peer reuses registerForward (same
	// admin-check → tg_botapi / tg_public / tg_mtproto paths and validation
	// as forwards). Without MTProto the option is not wired and /dialogs
	// degrades to the first-class "requires MTProto" refusal. The 30s
	// rate-limit + snapshot live in telegram.Bot (plan §4.6).
	if mtClient != nil {
		botOpts = append(botOpts, telegram.WithDialogLister(
			func(ctx context.Context, limit int) ([]mtproto.PeerInfo, error) {
				return mtClient.GetDialogs(ctx, limit)
			}))
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

// sourceAdder validates and persists a candidate source; the signature
// matches telegram.WithSourceAdder. freshCount is the number of posts seen by
// the trial collect (0 for kinds without one).
type sourceAdder func(ctx context.Context, kind, ref string) (id int64, freshCount int, err error)

// sourceExistsError is returned by the source adder when an ENABLED row for
// (kind, ref) already exists. It carries the row id so forward registration
// can render it as an informational "ℹ️" reply instead of an error (review
// L2); /addsource keeps its pre-Stage-8 "❌"-rendering of the same text.
type sourceExistsError struct{ id int64 }

func (e *sourceExistsError) Error() string {
	return fmt.Sprintf("источник уже добавлен (#%d)", e.id)
}

// newSourceAdder builds the /addsource closure. It validates a candidate
// source before persisting it, then inserts. Validation differs per kind:
//   - rss/tg_public: a trial collect MUST hit the network and return ≥1
//     item, with real errors (not "0 posts") surfaced on failure.
//   - tg_botapi: Bot API cannot pull channel history, so there is nothing
//     to trial-collect. Critically, a ManagedSource.Collect call DRAINS
//     the production channelBuf for that key — running it here would
//     silently discard real buffered posts. Admin-ness (checked upstream
//     in detectKind / forward extraction) is the only validation signal.
//   - tg_mtproto: trial GetHistoryByPeer through the secondary account
//     (see addMTProtoSource).
//
// Stage 8: persistence goes through AddSourceIfNotExists — the UNIQUE
// (kind, ref) index (migration v2) makes duplicate checks race-free and also
// covers DISABLED duplicates: a re-add after /removesource REACTIVATES the
// row (review H1) and is reported as success, not as "уже добавлен".
func newSourceAdder(store *storage.SQLiteStore, mtClient *mtproto.Client, hc *http.Client, tgLimit int) sourceAdder {
	return func(ctx context.Context, kind, ref string) (int64, int, error) {
		if kind == "tg_botapi" {
			id, created, reactivated, err := store.AddSourceIfNotExists(ctx, kind, ref)
			if err != nil {
				return 0, 0, err
			}
			if !created && !reactivated {
				return 0, 0, &sourceExistsError{id: id}
			}
			return id, 0, nil
		}

		if kind == "tg_mtproto" {
			id, created, reactivated, err := addMTProtoSource(ctx, store, mtClient, ref)
			if err != nil {
				return 0, 0, err
			}
			if !created && !reactivated {
				return 0, 0, &sourceExistsError{id: id}
			}
			// freshCount=0: the trial read (limit=1) validates access, it is
			// not a count of "fresh posts" (review L1).
			return id, 0, nil
		}

		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()

		var items []feed.Item
		var err error
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

		id, created, reactivated, err := store.AddSourceIfNotExists(ctx, kind, ref)
		if err != nil {
			return 0, 0, err
		}
		if !created && !reactivated {
			return 0, 0, &sourceExistsError{id: id}
		}
		return id, len(items), nil
	}
}

// newForwardRegistrar builds the closure that turns a forwarded message's
// origin into a source (plan Stage 8 A1). Order of preference: bot-admin
// chats are read directly through the Bot API (tg_botapi — works without
// MTProto, including private chats where the bot is admin); public non-admin
// channels fall back to t.me/s scraping (tg_public, validated by the source
// adder's trial collect); private chats need the MTProto secondary account
// (tg_mtproto, validated by a trial history read). All dynamic text is
// HTML-escaped — replies go out with ParseMode HTML.
func newForwardRegistrar(store *storage.SQLiteStore, mtClient *mtproto.Client, addSourceFn sourceAdder) telegram.ForwardRegistrar {
	return func(ctx context.Context, origin telegram.ForwardOrigin) string {
		title := telegram.EscapeHTML(origin.Title)

		switch {
		case origin.BotIsAdmin:
			ref := origin.Username
			if ref == "" {
				ref = strconv.FormatInt(origin.BotAPIChatID, 10)
			}
			id, created, reactivated, err := store.AddSourceIfNotExists(ctx, "tg_botapi", ref)
			if err != nil {
				slog.Error("forward registration failed", "kind", "tg_botapi", "err", err)
				return "❌ Внутренняя ошибка при добавлении источника."
			}
			switch {
			case reactivated:
				return fmt.Sprintf("✅ Источник #%d снова включён: %s.", id, title)
			case !created:
				return fmt.Sprintf("ℹ️ Источник уже добавлен (#%d): %s.", id, title)
			}
			return fmt.Sprintf("✅ Источник #%d добавлен: %s (бот читает посты напрямую).", id, title)

		case origin.Username != "":
			id, _, err := addSourceFn(ctx, "tg_public", origin.Username)
			if err != nil {
				var exists *sourceExistsError
				if errors.As(err, &exists) {
					return fmt.Sprintf("ℹ️ Источник уже добавлен (#%d): %s.", exists.id, title)
				}
				return "❌ " + telegram.EscapeHTML(err.Error())
			}
			return fmt.Sprintf("✅ Источник #%d добавлен: %s (публичный канал).", id, title)

		default:
			if mtClient == nil {
				return "Приватные чаты требуют настроенного MTProto (MTPROTO_APP_ID и MTPROTO_APP_HASH). " +
					"Сейчас он не сконфигурирован: перешлите сообщение из публичного канала " +
					"или добавьте бота в администраторы чата."
			}
			ref := strconv.FormatInt(origin.BotAPIChatID, 10)
			id, created, reactivated, err := addMTProtoSource(ctx, store, mtClient, ref)
			if err != nil {
				return "❌ " + telegram.EscapeHTML(err.Error())
			}
			switch {
			case reactivated:
				return fmt.Sprintf("✅ Источник #%d снова включён: %s.", id, title)
			case !created:
				return fmt.Sprintf("ℹ️ Источник уже добавлен (#%d): %s.", id, title)
			}
			return fmt.Sprintf("✅ Источник #%d добавлен: %s (приватный канал через MTProto).", id, title)
		}
	}
}

// mtprotoValidationTimeout bounds the trial GetHistoryByPeer used to validate
// a private source before persisting it (plan Stage 8 A1: 25s, FloodWait is
// handled inside the client).
const mtprotoValidationTimeout = 25 * time.Second

// addMTProtoSource validates a tg_mtproto ref (Bot API "-100X") with a trial
// history read through the secondary account, then persists the source via
// AddSourceIfNotExists. created=false && reactivated=false means an ENABLED
// (kind, ref) row already existed; reactivated=true means a disabled row was
// re-enabled (review H1). When MTProto is not configured the call fails with
// the degradation message — a first-class path, since the VPS may legitimately
// run without MTPROTO_APP_ID/HASH (user decision 2026-10-20).
func addMTProtoSource(ctx context.Context, store *storage.SQLiteStore, mtClient *mtproto.Client, ref string) (int64, bool, bool, error) {
	if mtClient == nil {
		return 0, false, false, fmt.Errorf("приватные источники требуют настроенного MTProto (MTPROTO_APP_ID и MTPROTO_APP_HASH)")
	}
	botAPIID, err := strconv.ParseInt(ref, 10, 64)
	if err != nil {
		return 0, false, false, fmt.Errorf("некорректный id приватного канала %q", ref)
	}
	x, ok := mtproto.ToMTProtoID(botAPIID)
	if !ok {
		return 0, false, false, fmt.Errorf("источник %q не является каналом или супергруппой", ref)
	}

	cctx, cancel := context.WithTimeout(ctx, mtprotoValidationTimeout)
	defer cancel()
	msgs, err := mtClient.GetHistoryByPeer(cctx, x, 1)
	if err != nil {
		// GetHistoryByPeer errors mention peer ids / RPC codes only — safe to
		// surface; no session data or access_hash is ever included.
		return 0, false, false, fmt.Errorf("источник недоступен (аккаунт не в этом чате?): %w", err)
	}
	if len(msgs) == 0 {
		return 0, false, false, fmt.Errorf("источник не вернул ни одного поста")
	}

	id, created, reactivated, err := store.AddSourceIfNotExists(ctx, "tg_mtproto", ref)
	if err != nil {
		return 0, false, false, err
	}
	return id, created, reactivated, nil
}
