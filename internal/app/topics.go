package app

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
	"github.com/akomyagin/aiTelegaBot/internal/llm"
	"github.com/akomyagin/aiTelegaBot/internal/storage"
	"github.com/akomyagin/aiTelegaBot/internal/telegram"
)

// Файл — app-сторона подэтапа B (Этап 8): регистрация статических источников
// в БД при старте и замыкания шести seams из internal/telegram
// (TopicLister/TopicCreator/TopicBinder/TopicSuggester/SourceRemoverV2/
// ForwardRegistrarV2 — последнее в app.go). Граница подэтапа: SourceID/TopicID
// на элементах дайджеста проставляет подэтап C, здесь — только регистрация и
// управление темами.

// staticSourceRef — координаты (kind, ref) одного статического источника из
// config для регистрации в БД (план, подэтап B: «Регистрация статических
// источников при старте»).
type staticSourceRef struct {
	Kind string
	Ref  string
}

// registerStaticSources регистрирует статические источники из config в таблице
// sources с origin='static' — ДО первого вызова sourceProvider, чтобы /topics,
// /settopic и /removesource видели их как обычные строки. UpsertStaticSource
// идемпотентен (ON CONFLICT не трогает topic_id) — повторный старт не затирает
// привязку темы. Ошибка одного источника логируется и НЕ фатальна: сервис
// стартует, статический слой собирается из config независимо от БД.
func registerStaticSources(ctx context.Context, store *storage.SQLiteStore, refs []staticSourceRef) {
	for _, r := range refs {
		if err := store.UpsertStaticSource(ctx, r.Kind, r.Ref); err != nil {
			slog.Warn("static source registration failed", "kind", r.Kind, "ref", r.Ref, "err", err)
		}
	}
}

// newTopicLister собирает telegram.TopicLister: ListTopics + ListSources и
// одна map-группировка источников по topic_id (storage уже читает
// COALESCE(topic_id,1), NULL приходит как 1). Без N+1: ровно два запроса.
func newTopicLister(store *storage.SQLiteStore) telegram.TopicLister {
	return func(ctx context.Context) ([]telegram.TopicView, error) {
		topics, err := store.ListTopics(ctx)
		if err != nil {
			return nil, err
		}
		sources, err := store.ListSources(ctx)
		if err != nil {
			return nil, err
		}
		byTopic := make(map[int64][]telegram.TopicSourceView, len(topics))
		for _, s := range sources {
			byTopic[s.TopicID] = append(byTopic[s.TopicID], telegram.TopicSourceView{
				ID:      s.ID,
				Kind:    s.Kind,
				Ref:     s.Ref,
				Enabled: s.Enabled,
			})
		}
		views := make([]telegram.TopicView, 0, len(topics))
		for _, t := range topics {
			views = append(views, telegram.TopicView{
				ID:      t.ID,
				Name:    t.Name,
				Sources: byTopic[t.ID],
			})
		}
		return views, nil
	}
}

// newTopicCreator собирает telegram.TopicCreator. storage.CreateTopic
// идемпотентен и возвращает только id, поэтому Created («создана»/«уже была»)
// различается здесь: имя сверяется с ListTopics ДО вставки (контракт
// b-tg §2; single-user сервис, гонка непрактична).
func newTopicCreator(store *storage.SQLiteStore) telegram.TopicCreator {
	return func(ctx context.Context, name string) (telegram.NewTopicResult, error) {
		topics, err := store.ListTopics(ctx)
		if err != nil {
			return telegram.NewTopicResult{}, err
		}
		existed := false
		for _, t := range topics {
			if t.Name == name {
				existed = true
				break
			}
		}
		id, err := store.CreateTopic(ctx, name)
		if err != nil {
			return telegram.NewTopicResult{}, err
		}
		return telegram.NewTopicResult{ID: id, Created: !existed}, nil
	}
}

// newTopicBinder собирает telegram.TopicBinder (/settopic и callback ts:).
// topicArg: числовая строка → id темы; иначе ТОЧНОЕ имя → разрешение через
// ListTopics. Существование источника И темы валидируется здесь, потому что
// storage.SetSourceTopic не валидирует topicID (PRAGMA foreign_keys выключен,
// см. отчёт b-core). TopicName — имя разрешённой темы для ответа.
func newTopicBinder(store *storage.SQLiteStore) telegram.TopicBinder {
	return func(ctx context.Context, sourceID int64, topicArg string) (telegram.TopicBindResult, error) {
		sources, err := store.ListSources(ctx)
		if err != nil {
			return telegram.TopicBindResult{}, err
		}
		sourceFound := false
		for _, s := range sources {
			if s.ID == sourceID {
				sourceFound = true
				break
			}
		}
		if !sourceFound {
			return telegram.TopicBindResult{SourceFound: false}, nil
		}

		topics, err := store.ListTopics(ctx)
		if err != nil {
			return telegram.TopicBindResult{}, err
		}
		var (
			topicID   int64
			topicName string
		)
		if id, perr := strconv.ParseInt(strings.TrimSpace(topicArg), 10, 64); perr == nil {
			for _, t := range topics {
				if t.ID == id {
					topicID, topicName = t.ID, t.Name
					break
				}
			}
			if topicID == 0 {
				// Ревью L2: числовой topicArg трактуется как id, но тема с
				// числовым именем («2024») иначе недостижима — при отсутствии
				// темы с таким id fallback на ТОЧНОЕ совпадение по имени.
				for _, t := range topics {
					if t.Name == topicArg {
						topicID, topicName = t.ID, t.Name
						break
					}
				}
			}
		} else {
			for _, t := range topics {
				if t.Name == topicArg {
					topicID, topicName = t.ID, t.Name
					break
				}
			}
		}
		if topicID == 0 {
			return telegram.TopicBindResult{SourceFound: true, TopicFound: false}, nil
		}

		found, err := store.SetSourceTopic(ctx, sourceID, topicID)
		if err != nil {
			return telegram.TopicBindResult{}, err
		}
		return telegram.TopicBindResult{
			SourceFound: found,
			TopicFound:  true,
			TopicID:     topicID,
			TopicName:   topicName,
		}, nil
	}
}

// Таймаут suggested-потока (§4.11): 15с на LLM-запрос SuggestTopic.
// Отдельного sample-таймаута больше нет (ревью M3): sample — trial-элементы,
// собранные при валидации самого добавления (SourceAddResult/ForwardResult.
// Sample), второй сетевой сбор не выполняется. collectTopicSample и
// topicSampleTimeout/topicSampleLimit удалены — единственный потребитель
// исчез.
const topicSuggestTimeout = 15 * time.Second

// normalizeTopicName — нормализация имени темы для case-insensitive сверки
// (ревью L1): lower + trim пробелов по краям.
func normalizeTopicName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// newTopicSuggester собирает telegram.TopicSuggester (§4.11): sample
// (trial-элементы из пути добавления — ревью M3, без повторного сбора) +
// existing (имена тем, «Без темы» исключается) → llm.SuggestTopic с
// таймаутом 15с → при непустом имени CreateTopic СРАЗУ → &TopicSuggestion.
// ЛЮБОЙ отказ (ошибка, пустое имя) → nil: telegram доставит старый ответ
// без кнопок, добавление не падает. Ошибки логируются на Warn — предложение
// темы не фатально.
func newTopicSuggester(
	store *storage.SQLiteStore,
	suggester llm.TopicSuggester,
) telegram.TopicSuggester {
	return func(ctx context.Context, sourceID int64, kind, ref string, sample []feed.Item) *telegram.TopicSuggestion {
		if sourceID == 0 || suggester == nil {
			return nil
		}

		// existing — имена для промпта (сигнатура llm.SuggestTopic не
		// меняется), byNorm — id по нормализованному имени для сверки (L1).
		var existing []string
		byNorm := make(map[string]storage.Topic)
		if topics, err := store.ListTopics(ctx); err != nil {
			slog.Warn("topic suggestion: ListTopics failed", "err", err)
			return nil
		} else {
			for _, t := range topics {
				// Seed-тема «Без темы» (id=1) — не кандидат для предложения.
				if t.ID != 1 {
					existing = append(existing, t.Name)
					byNorm[normalizeTopicName(t.Name)] = t
				}
			}
		}

		sctx, cancel := context.WithTimeout(ctx, topicSuggestTimeout)
		defer cancel()
		name, err := suggester.SuggestTopic(sctx, existing, sample)
		if err != nil {
			slog.Warn("topic suggestion failed", "kind", kind, "err", err)
			return nil
		}
		name = strings.TrimSpace(name)
		if name == "" {
			return nil // «предложить нечего» — не ошибка
		}

		// Ревью L1: модель могла вернуть имя, отличающееся от существующего
		// только регистром/пробелами («AI News» vs «ai news») — UNIQUE точный,
		// CreateTopic создал бы near-duplicate. Совпадение по нормализованной
		// строке → возвращаем существующую тему, не создавая новую.
		if t, ok := byNorm[normalizeTopicName(name)]; ok {
			return &telegram.TopicSuggestion{TopicID: t.ID, TopicName: t.Name}
		}

		id, err := store.CreateTopic(ctx, name)
		if err != nil {
			slog.Warn("topic suggestion: CreateTopic failed", "name", name, "err", err)
			return nil
		}
		return &telegram.TopicSuggestion{TopicID: id, TopicName: name}
	}
}

// newSourceRemoverV2 собирает origin-aware версию /removesource (контракт
// b-tg §5): static-строка → {Found:true, Static:true} БЕЗ отключения
// (статические источники управляются через .env); dynamic → DisableSource
// как в A1. Поиск по id — через ListSources (отдельного GetSource в storage
// нет; таблица маленькая, single-user).
func newSourceRemoverV2(store *storage.SQLiteStore) telegram.SourceRemoverV2 {
	return func(ctx context.Context, id int64) (telegram.SourceRemoveResult, error) {
		sources, err := store.ListSources(ctx)
		if err != nil {
			return telegram.SourceRemoveResult{}, err
		}
		for _, s := range sources {
			if s.ID == id && s.Origin == "static" {
				return telegram.SourceRemoveResult{Found: true, Static: true}, nil
			}
		}
		found, err := store.DisableSource(ctx, id)
		if err != nil {
			return telegram.SourceRemoveResult{}, err
		}
		return telegram.SourceRemoveResult{Found: found}, nil
	}
}
