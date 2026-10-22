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
	"github.com/akomyagin/aiTelegaBot/internal/linkfetch"
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

	// LLM: BYOK — offline summarizer when no key is configured. Stage 9A:
	// pipeline потребляет вызов A (ClassifySummarizer); *llm.Client и
	// *llm.Offline реализуют его. Этап 9D-2b: старый TopicSummarizer
	// (llm/topicdigest.go, Этап 8) удалён — вызов C (AggregateTopic) заменил
	// его в Run; вызовы C/D подключаются через newAggregators (та же вилка).
	if cfg.Offline {
		slog.Info("no LLM API key configured; using offline summarizer")
	}
	summarizer := newSummarizer(cfg)

	// Static sources from config. Built once at startup: some carry per-run
	// state (channelBuf, mtClient), so they are reused across runs. Dynamic,
	// DB-backed sources are (re)built on every run — see sourceProvider below.
	var staticSources []feed.Source
	// staticRefs — те же источники координатами (kind, ref) для регистрации в
	// БД (подэтап B). Собирается ВМЕСТЕ со staticSources, чтобы в БД не попало
	// то, что фактически не строится (например MTPROTO_CHANNELS без mtClient).
	// Правила kind/ref (план 609–614, множество kind БД = полное множество
	// feed.Kind):
	//   - RSS-фиды → (rss, <url>); arXiv-фид → (arxiv, <url>) — вид определяет
	//     ТО ЖЕ правило, что и у статического слоя: вхождение "arxiv.org" в URL
	//     (arXiv API отдаёт Atom по обычному URL, отдельного ident нет);
	//   - HN → (hn, "frontpage") — единственный источник, limit не часть ref;
	//   - managed TG-каналы → (tg_botapi, <username без @>) — та же форма ref,
	//     что использует forward-регистрация для tg_botapi (username, либо
	//     числовой "-100X" для приватных чатов);
	//   - публичные TG-каналы → (tg_public, <username без @>);
	//   - MTProto-каналы → (tg_mtproto, <username без @>) — форма @username
	//     ветки ChannelSource в buildDynamicSources.
	var staticRefs []staticSourceRef
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
		staticRefs = append(staticRefs, staticSourceRef{Kind: kind, Ref: u})
	}
	if cfg.HNLimit > 0 {
		staticSources = append(staticSources, feed.NewHNSource("Hacker News", cfg.HNLimit, hc))
		staticRefs = append(staticRefs, staticSourceRef{Kind: "hn", Ref: "frontpage"})
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
		staticRefs = append(staticRefs, staticSourceRef{Kind: "tg_botapi", Ref: ch})
	}
	for _, ch := range cfg.TGPublicChannels {
		ch = strings.TrimPrefix(strings.TrimSpace(ch), "@")
		if ch == "" {
			continue
		}
		staticSources = append(staticSources, telegram.NewPublicSource("@"+ch, ch, hc, tgLimit))
		staticRefs = append(staticRefs, staticSourceRef{Kind: "tg_public", Ref: ch})
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
			staticRefs = append(staticRefs, staticSourceRef{Kind: "tg_mtproto", Ref: ch})
		}
	}

	// Stage 8 B: статические источники регистрируются в БД (origin='static')
	// ДО sourceProvider — чтобы /topics,/settopic,/removesource видели их как
	// обычные строки. Ошибка upsert одного источника не фатальна (лог внутри).
	registerStaticSources(ctx, store, staticRefs)

	// Ревью M4: pruning осиротевших static-строк — источники, исчезнувшие из
	// config (например URL убрали из .env), иначе навсегда остались бы в БД
	// и засоряли /topics. Ошибка pruning'а не фатальна (Warn).
	// Keep-set включает MTPROTO_CHANNELS даже когда mtClient==nil: деградный
	// запуск их не регистрирует, но и удалять существующие строки нельзя —
	// с MTProto-переменными в следующем запуске каналы вернутся, а pruning
	// необратимо потерял бы их привязку topic_id.
	keep := make([]storage.StaticRef, 0, len(staticRefs)+len(cfg.MTProtoChannels))
	for _, r := range staticRefs {
		keep = append(keep, storage.StaticRef{Kind: r.Kind, Ref: r.Ref})
	}
	if mtClient == nil {
		for _, ch := range cfg.MTProtoChannels {
			ch = strings.TrimPrefix(strings.TrimSpace(ch), "@")
			if ch == "" {
				continue
			}
			keep = append(keep, storage.StaticRef{Kind: "tg_mtproto", Ref: ch})
		}
	}
	if removed, err := store.PruneStaticSources(ctx, keep); err != nil {
		slog.Warn("static source pruning failed", "err", err)
	} else if removed > 0 {
		slog.Info("pruned orphaned static sources", "removed", removed)
	}

	// sourceProvider returns the tracked source set for each run: the static
	// config layer (SourceID/TopicID из строк origin='static' по (kind, ref))
	// plus a fresh snapshot of enabled DB-backed sources. Called once per Run,
	// so sources added via /addsource are picked up without a restart. Stage 8
	// C: одна выборка ListSources на прогон (newSourceProvider, тест-кейс 7).
	sourceProvider := newSourceProvider(store, staticSources, staticRefs,
		hc, tgLimit, channelBuf, mtClient, mtLimit)

	// Этап 9B: вызовы E/F и fetcher внешних ссылок — та же offline-вилка, что
	// и у newSummarizer (паттерн wiring Этапа 9A).
	linkClassify, linkSummary, linkFetch := newLinkStack(cfg)
	// Этап 9D-2b: вызовы C/D (агрегация саммари) — та же offline-вилка.
	topicAgg, overallAgg := newAggregators(cfg)
	pipeline := &digest.Pipeline{
		SourceProvider:    sourceProvider,
		Store:             store,
		Classify:          summarizer,
		Dedup:             newDeduper(cfg),
		Aggregate:         topicAgg,
		Overall:           overallAgg,
		LinkClassify:      linkClassify,
		LinkSummary:       linkSummary,
		LinkFetch:         linkFetch,
		LinkPerMessageMax: cfg.LinkPerMessageMax,
		LinkFetchTotalMax: cfg.LinkFetchTotalMax,
		ChatID:            cfg.TelegramChatID,
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
			// Static-строки (подэтап B) уже показаны секцией выше из config —
			// без фильтра они дублировались бы в «динамических».
			if ds.Origin == "static" {
				continue
			}
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
		// Stage 8 B: V2-опции имеют приоритет над legacy (b-tg §5–6), поэтому
		// legacy WithSourceRemover/WithForwardRegistrar здесь больше не нужны.
		telegram.WithSourceRemoverV2(newSourceRemoverV2(store)),
		telegram.WithForwardRegistrarV2(registerForward),
		telegram.WithTopicLister(newTopicLister(store)),
		telegram.WithTopicCreator(newTopicCreator(store)),
		telegram.WithTopicBinder(newTopicBinder(store)),
		// Stage 8 C: T:/TOC:-навигация дайджеста — данные строго из SQLite
		// (digest_nav.go), поэтому опция проводится всегда (store есть в
		// любом режиме) и переживает рестарт бота.
		telegram.WithDigestViewer(newDigestViewer(store)),
		// Stage 8 D: L2:/L3: drill-down — данные строго из SQLite, как у
		// C-viewer (digest_nav.go), поэтому опция проводится всегда.
		// sources-снапшот = store.ListSources: один вызов на рендер
		// drill-down, БЕЗ кэша — сервис single-user, таблица sources
		// маленькая, свежее имя важнее экономии запроса.
		telegram.WithDrillDown(digest.NewDrillDown(store, store.ListSources)),
	}
	// Suggested-поток (§4.11, ревью M1): опция проводится ТОЛЬКО для
	// сетевого LLM-клиента (*llm.Client). llm.Offline тоже реализует
	// TopicSuggester (всегда («», nil)), но type-assert на интерфейс
	// проводил бы seam и для него; offline → опция не проводится → ответ
	// добавления без кнопок и НОЛЬ дополнительных сетевых вызовов.
	if lc, ok := summarizer.(*llm.Client); ok {
		botOpts = append(botOpts, telegram.WithTopicSuggestion(
			newTopicSuggester(store, lc)))
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
	pipeline.Poster = bot

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

// newSummarizer выбирает реализацию вызова A (Этап 9A, план §4.8):
// cfg.Offline → детерминированный *llm.Offline (без сети), иначе сетевой
// *llm.Client. Паттерн прежнего выбора TopicSummarizer (Stage 8 C); вынесено
// из Run ради тест-кейса 7 (wiring проверяется без подъёма бота/планировщика).
func newSummarizer(cfg *config.Config) llm.ClassifySummarizer {
	if cfg.Offline {
		return llm.NewOffline()
	}
	return llm.NewClient(cfg.LLMAPIKey, cfg.LLMBaseURL, cfg.LLMModel, cfg.LLMMaxRetries)
}

// newDeduper выбирает реализацию вызова B (Этап 9C, план §4.4): cfg.Offline →
// детерминированный *llm.Offline (точное совпадение нормализованного
// заголовка, без сети), иначе сетевой *llm.Client. Та же offline-вилка, что и
// у newSummarizer/newLinkStack (паттерн wiring Этапа 9A).
func newDeduper(cfg *config.Config) llm.Deduper {
	if cfg.Offline {
		return llm.NewOffline()
	}
	return llm.NewClient(cfg.LLMAPIKey, cfg.LLMBaseURL, cfg.LLMModel, cfg.LLMMaxRetries)
}

// newAggregators выбирает реализации вызовов C/D (Этап 9D, план §4.7/§4.8):
// cfg.Offline → детерминированный *llm.Offline (fallback-строки §4.7, без
// сети), иначе сетевой *llm.Client. Та же offline-вилка и тот же клиент, что
// у newSummarizer/newDeduper/newLinkStack (паттерн wiring Этапа 9A): один
// *llm.Client на все вызовы прогона.
func newAggregators(cfg *config.Config) (llm.TopicAggregator, llm.OverallAggregator) {
	if cfg.Offline {
		off := llm.NewOffline()
		return off, off
	}
	cl := llm.NewClient(cfg.LLMAPIKey, cfg.LLMBaseURL, cfg.LLMModel, cfg.LLMMaxRetries)
	return cl, cl
}

// newLinkStack — wiring шага 4c (Этап 9B, план §4.3): вызов E (LinkClassifier)
// и вызов F (LinkSummarizer) реализуются теми же *llm.Client / *llm.Offline,
// что и вызов A; fetcher создаётся ТОЛЬКО в сетевом режиме (offline → nil:
// fetch-фаза пропускается, item_links пишутся с kind=other). http.Client у
// fetcher ОТДЕЛЬНЫЙ (не общий с feed): собственный Timeout из
// cfg.LinkFetchTimeout — осознанное отклонение от формулировки плана «общий
// hc», к лучшему: таймаут ссылок (дефолт 15с) независим от таймаута feed
// (pipeline ctx сам не оборачивает; Fetch уважает отмену ctx).
// LinkFetchTimeout ≤ 0 возможен лишь при ручном конструировании Config
// (Load даёт дефолт 15с) — страховка дефолтом.
func newLinkStack(cfg *config.Config) (llm.LinkClassifier, llm.LinkSummarizer, digest.LinkFetcher) {
	if cfg.Offline {
		off := llm.NewOffline()
		return off, off, nil
	}
	cl := llm.NewClient(cfg.LLMAPIKey, cfg.LLMBaseURL, cfg.LLMModel, cfg.LLMMaxRetries)
	timeout := cfg.LinkFetchTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	hc := &http.Client{Timeout: timeout}
	return cl, cl, linkfetch.NewFetcher(hc, cfg.LinkFetchMaxBytes)
}

// sourceAdder validates and persists a candidate source; the signature
// matches telegram.WithSourceAdder. Результат — telegram.SourceAddResult:
// id строки, Fresh (число постов trial-collect'а, 0 для видов без него) и
// Sample — trial-элементы валидации для suggested-потока (ревью M3: второй
// сетевой сбор не выполняется).
type sourceAdder func(ctx context.Context, kind, ref string) (telegram.SourceAddResult, error)

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
	return func(ctx context.Context, kind, ref string) (telegram.SourceAddResult, error) {
		if kind == "tg_botapi" {
			id, created, reactivated, err := store.AddSourceIfNotExists(ctx, kind, ref)
			if err != nil {
				return telegram.SourceAddResult{}, err
			}
			if !created && !reactivated {
				return telegram.SourceAddResult{}, &sourceExistsError{id: id}
			}
			// Sample всегда пуст: Collect ManagedSource дренирует production
			// channelBuf (план §4.11) — терять реальные посты нельзя.
			return telegram.SourceAddResult{ID: id}, nil
		}

		if kind == "tg_mtproto" {
			id, created, reactivated, sample, err := addMTProtoSource(ctx, store, mtClient, ref)
			if err != nil {
				return telegram.SourceAddResult{}, err
			}
			if !created && !reactivated {
				return telegram.SourceAddResult{}, &sourceExistsError{id: id}
			}
			// Fresh=0: the trial read (limit=1) validates access, it is
			// not a count of "fresh posts" (review L1). Sample — тот самый
			// trial-элемент (ревью M3: без второго GetHistory).
			return telegram.SourceAddResult{ID: id, Sample: sample}, nil
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
			return telegram.SourceAddResult{}, fmt.Errorf("неизвестный вид источника %q", kind)
		}
		if err != nil {
			return telegram.SourceAddResult{}, fmt.Errorf("источник недоступен: %w", err)
		}
		if len(items) == 0 {
			return telegram.SourceAddResult{}, fmt.Errorf("источник не вернул ни одного поста (проверьте адрес/канал)")
		}

		id, created, reactivated, err := store.AddSourceIfNotExists(ctx, kind, ref)
		if err != nil {
			return telegram.SourceAddResult{}, err
		}
		if !created && !reactivated {
			return telegram.SourceAddResult{}, &sourceExistsError{id: id}
		}
		// Trial-элементы валидации переиспользуются как sample темы (M3).
		return telegram.SourceAddResult{ID: id, Fresh: len(items), Sample: items}, nil
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
//
// Stage 8 B: возвращает ForwardRegistrarV2 — вместе с текстом координаты
// сохранённого источника (SourceID/Kind/Ref) для suggested-потока (§4.11).
// SourceID != 0 ТОЛЬКО при успешном добавлении (создан или ре-активирован);
// «уже добавлен» (ℹ️), отказы и ошибки → SourceID=0, кнопок темы не будет.
func newForwardRegistrar(store *storage.SQLiteStore, mtClient *mtproto.Client, addSourceFn sourceAdder) telegram.ForwardRegistrarV2 {
	return func(ctx context.Context, origin telegram.ForwardOrigin) telegram.ForwardResult {
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
				return telegram.ForwardResult{Reply: "❌ Внутренняя ошибка при добавлении источника."}
			}
			switch {
			case reactivated:
				return telegram.ForwardResult{
					Reply:    fmt.Sprintf("✅ Источник #%d снова включён: %s.", id, title),
					SourceID: id, Kind: "tg_botapi", Ref: ref,
				}
			case !created:
				return telegram.ForwardResult{
					Reply: fmt.Sprintf("ℹ️ Источник уже добавлен (#%d): %s.", id, title),
				}
			}
			return telegram.ForwardResult{
				Reply:    fmt.Sprintf("✅ Источник #%d добавлен: %s (бот читает посты напрямую).", id, title),
				SourceID: id, Kind: "tg_botapi", Ref: ref,
			}

		case origin.Username != "":
			res, err := addSourceFn(ctx, "tg_public", origin.Username)
			if err != nil {
				var exists *sourceExistsError
				if errors.As(err, &exists) {
					return telegram.ForwardResult{
						Reply: fmt.Sprintf("ℹ️ Источник уже добавлен (#%d): %s.", exists.id, title),
					}
				}
				return telegram.ForwardResult{Reply: "❌ " + telegram.EscapeHTML(err.Error())}
			}
			return telegram.ForwardResult{
				Reply:    fmt.Sprintf("✅ Источник #%d добавлен: %s (публичный канал).", res.ID, title),
				SourceID: res.ID, Kind: "tg_public", Ref: origin.Username,
				Sample: res.Sample, // trial-элементы валидации (ревью M3)
			}

		default:
			if mtClient == nil {
				return telegram.ForwardResult{Reply: "Приватные чаты требуют настроенного MTProto (MTPROTO_APP_ID и MTPROTO_APP_HASH). " +
					"Сейчас он не сконфигурирован: перешлите сообщение из публичного канала " +
					"или добавьте бота в администраторы чата."}
			}
			ref := strconv.FormatInt(origin.BotAPIChatID, 10)
			id, created, reactivated, sample, err := addMTProtoSource(ctx, store, mtClient, ref)
			if err != nil {
				return telegram.ForwardResult{Reply: "❌ " + telegram.EscapeHTML(err.Error())}
			}
			switch {
			case reactivated:
				return telegram.ForwardResult{
					Reply:    fmt.Sprintf("✅ Источник #%d снова включён: %s.", id, title),
					SourceID: id, Kind: "tg_mtproto", Ref: ref, Sample: sample,
				}
			case !created:
				return telegram.ForwardResult{
					Reply: fmt.Sprintf("ℹ️ Источник уже добавлен (#%d): %s.", id, title),
				}
			}
			return telegram.ForwardResult{
				Reply:    fmt.Sprintf("✅ Источник #%d добавлен: %s (приватный канал через MTProto).", id, title),
				SourceID: id, Kind: "tg_mtproto", Ref: ref,
				Sample: sample, // trial-элемент валидационного чтения (ревью M3)
			}
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
// Stage 8 B (ревью M3): возвращает также sample — trial-элемент из уже
// прочитанного при валидации сообщения (limit=1) для suggested-потока;
// второй GetHistory не выполняется (лишний FloodWait-риск), одного элемента
// достаточно для предложения темы по заголовку.
func addMTProtoSource(ctx context.Context, store *storage.SQLiteStore, mtClient *mtproto.Client, ref string) (int64, bool, bool, []feed.Item, error) {
	if mtClient == nil {
		return 0, false, false, nil, fmt.Errorf("приватные источники требуют настроенного MTProto (MTPROTO_APP_ID и MTPROTO_APP_HASH)")
	}
	botAPIID, err := strconv.ParseInt(ref, 10, 64)
	if err != nil {
		return 0, false, false, nil, fmt.Errorf("некорректный id приватного канала %q", ref)
	}
	x, ok := mtproto.ToMTProtoID(botAPIID)
	if !ok {
		return 0, false, false, nil, fmt.Errorf("источник %q не является каналом или супергруппой", ref)
	}

	cctx, cancel := context.WithTimeout(ctx, mtprotoValidationTimeout)
	defer cancel()
	msgs, err := mtClient.GetHistoryByPeer(cctx, x, 1)
	if err != nil {
		// GetHistoryByPeer errors mention peer ids / RPC codes only — safe to
		// surface; no session data or access_hash is ever included.
		return 0, false, false, nil, fmt.Errorf("источник недоступен (аккаунт не в этом чате?): %w", err)
	}
	if len(msgs) == 0 {
		return 0, false, false, nil, fmt.Errorf("источник не вернул ни одного поста")
	}
	sample := []feed.Item{mtproto.TrialItem(msgs[0], ref)}

	id, created, reactivated, err := store.AddSourceIfNotExists(ctx, "tg_mtproto", ref)
	if err != nil {
		return 0, false, false, nil, err
	}
	return id, created, reactivated, sample, nil
}
