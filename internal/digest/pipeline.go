// Package digest orchestrates one digest run (Stage 8 C, plan §C/§4.11a;
// Stage 9A, план §4.8.1): collect items from all tracked sources → filter
// unseen → read the message_state processing cache → group by topic →
// classify+summarize uncached items via llm.ClassifySummarizer (вызов A) →
// semantic dedup of freshly classified items via llm.Deduper (вызов B, Этап
// 9C: non-primaries → dropped_dup + seen immediately; кластер с хотя бы одним
// degraded-членом НЕ персистится и его дубли не seen — следующий прогон
// пересмотрит) →
// drop ads (seen immediately) → persist per-topic digests → deliver ONE
// table-of-contents message with per-topic buttons → mark delivered.
// See docs/plans/stage-9-bottom-up-digest.md «Подэтап 9A».
//
// Delivery gates persistence (§4.3): rows are inserted with delivered=0 and
// flipped by MarkDelivered only after the TOC message reaches the user, so a
// failed delivery is retried (in full) on the next run. The message_state
// cache is the exception (§4.9): LLM answers are persisted BEFORE delivery so
// a crash never loses LLM work — the retry rebuilds the digest from the cache
// without re-asking the LLM. Ads are marked seen before (and independent of)
// delivery: they are never shown, so there is nothing to lose.
package digest

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
	"github.com/akomyagin/aiTelegaBot/internal/llm"
	"github.com/akomyagin/aiTelegaBot/internal/storage"
	"github.com/akomyagin/aiTelegaBot/internal/telegram"
)

// MessagePoster delivers the TOC message with its inline keyboard. Satisfied
// by *telegram.Bot (DeliverWithButtons).
type MessagePoster interface {
	DeliverWithButtons(ctx context.Context, chatID, text string,
		buttons [][]telegram.InlineButton) error
}

// Pipeline holds the dependencies for one digest run.
type Pipeline struct {
	// SourceProvider returns the tracked source set for THIS run. It is called
	// at the start of every Run, so DB-backed sources added at runtime are
	// picked up without a restart. Must be non-nil.
	SourceProvider func(ctx context.Context) ([]TrackedSource, error)
	Store          storage.Store
	// Classify — вызов A плана §4.8 (классификация + саммари сообщения,
	// Этап 9A). Заменил TopicSummarizer в Run; llm/topicdigest.go удалён в
	// 9D-2b, заменён вызовом C (§4.7, llm/aggregate.go).
	Classify llm.ClassifySummarizer
	// Поля шага 4c (Этап 9B, план §4.3/§4.8.1): внешние ссылки kept-элементов.
	// Нил-сейвы: LinkClassify == nil → шаг 4c пропускается ЦЕЛИКОМ (обратная
	// совместимость с pipeline-тестами 9A); LinkFetch == nil → fetch-фаза
	// пропускается (offline: item_links пишутся, саммари статей нет).
	LinkClassify llm.LinkClassifier // вызов E — виды ссылок, батч на тему
	LinkSummary  llm.LinkSummarizer // вызов F — саммари свежесобранных страниц
	LinkFetch    LinkFetcher        // ≤1 GET на URL; nil в offline
	// Dedup — вызов B плана §4.4/§4.8 (семантический дедуп, Этап 9C).
	// Нил-сейф: nil → дедуп пропускается ЦЕЛИКОМ, все элементы primary
	// (обратная совместимость с pipeline-тестами 9A/9B). Ошибка B — деградация
	// (log.Warn + все primary), тема НЕ пропускается — в отличие от ошибки A.
	Dedup llm.Deduper
	// Aggregate — вызов C плана §4.8 (headline темы + саммари топиков, Этап
	// 9D, шаг 4d). Нил-сейв: nil → вызов C пропускается, headline пуст
	// (fallback-ветка RenderTopicBody с заголовками элементов — байт-в-байт
	// тело темы 9A–9C), sections_json пуст. ОШИБКА ТРАНСПОРТА C — НЕ пропуск
	// темы (в отличие от ошибки A): headline = fallback из первых ≤3 саммари
	// списком «- …» (§4.7, тест-кейс 8 подэтапа 9D), sections пусты.
	Aggregate llm.TopicAggregator
	// Overall — вызов D плана §4.8 (общее саммари выпуска, шаг 6, ОДИН на
	// прогон). Нил-сейв: nil → TOC без overall-блока (поведение 9A–9C). Сбой
	// (error ИЛИ пустой summary) — log-only деградация: строка runs.summary
	// НЕ пишется, TOC доставляется без общего саммари, Run продолжается
	// (§4.8.1 шаг 6 — деградация, не ошибка).
	Overall llm.OverallAggregator
	// LinkPerMessageMax — ≤N ссылок на сообщение (LINK_PER_MESSAGE_MAX,
	// дефолт 5); LinkFetchTotalMax — бюджет fetch'ей ОБЩИЙ НА ПРОГОН
	// (LINK_FETCH_TOTAL_MAX, дефолт 30); сверх бюджета ссылки остаются в
	// item_links без саммари (деградация, не ошибка).
	LinkPerMessageMax int
	LinkFetchTotalMax int
	Poster            MessagePoster
	ChatID            string
	Log               *slog.Logger     // optional; defaults to slog.Default()
	Now               func() time.Time // optional; defaults to time.Now (for tests)
}

// log returns the configured logger or the default.
func (p *Pipeline) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

// Статусы message_state (план §4.2, Этап 9A пишет done/dropped_ad).
const (
	stateDone       = "done"
	stateDroppedAd  = "dropped_ad"
	stateDroppedDup = "dropped_dup" // non-primary вызова B (9C); из кеша — с 9A
)

// isClassifiedState сообщает, что элемент НЕ требует LLM-работы в этом прогоне:
// done (классифицирован+саммаризован) или dropped_* (окончательный сброс).
// status=new/failed и отсутствие строки → вызов A (§4.9: failed не блокирует
// следующий прогон).
func isClassifiedState(status string) bool {
	return status == stateDone || status == stateDroppedAd || status == stateDroppedDup
}

// messageStateFromClassify строит строку кеша message_state по одному
// результату вызова A. TopicID — НОРМАЛИЗОВАННЫЙ id темы прогона (L3:
// it.TopicID==0 → 1 «Без темы»), а не сырой штамп элемента. Degraded-элементы
// фильтрует вызывающий (им status=done не пишется — §4.9).
func messageStateFromClassify(it feed.Item, tid int64, r llm.ItemClassSummary) storage.MessageState {
	status := stateDone
	if r.Class == llm.ClassAd {
		status = stateDroppedAd
	}
	return storage.MessageState{
		DedupKey: it.DedupKey(),
		SourceID: it.SourceID,
		TopicID:  tid,
		Class:    r.Class,
		Summary:  r.Summary,
		// A-батч пишет всё primary с пустым cluster_id; члены кластеров
		// перезаписываются вторым B-батчем вызова B (9C, шаг 4a-bis).
		IsPrimary: true,
		Status:    status,
	}
}

// chooseDupPrimary — детерминированный выбор первоисточника среди членов
// кластера вызова B (критерий §4.4): самая ранняя Published → самый полный
// текст → меньший SourceID. Нулевой Published трактуется как ПОЗДНЕЙШИЙ
// (фикс 9C-ревью: элемент без даты проигрывает любому датированному; два
// нулевых — решаются следующими критериями). Используется защитной веткой
// переназначения primary=ad (в 9C недостижима — B идёт после A, ad исключены
// из входа).
// indices — 1-based номера в items (уже валидированы: ⊆ 1..len(items)).
func chooseDupPrimary(items []llm.DedupItem, indices []int) int {
	best := indices[0]
	for _, v := range indices[1:] {
		a, b := items[best-1], items[v-1]
		switch {
		case !a.Published.Equal(b.Published):
			switch {
			case a.Published.IsZero(): // best без даты — «позднейший»: датированный претендент побеждает
				best = v
			case b.Published.IsZero(): // претендент без даты проигрывает датированному best
			case b.Published.Before(a.Published):
				best = v
			}
		case len([]rune(b.Text)) != len([]rune(a.Text)):
			if len([]rune(b.Text)) > len([]rune(a.Text)) {
				best = v
			}
		case b.SourceID < a.SourceID:
			best = v
		}
	}
	return best
}

// dupClusterValid — L4-guard контракта Deduper (фикс 9C-ревью): все Indices в
// 1..n, без дублей индексов внутри кластера, Primary ∈ Indices. Прод-реализации
// гарантируют это парсером-валидатором (parseDedupResponse); guard защищает от
// третьей реализации или багованного фейка — невалидный кластер отбрасывается
// ЦЕЛИКОМ (log.Error у вызывающего, не panic), тема продолжается с остальными.
func dupClusterValid(cl llm.DupCluster, n int) bool {
	seen := make(map[int]struct{}, len(cl.Indices))
	hasPrimary := false
	for _, v := range cl.Indices {
		if v < 1 || v > n {
			return false
		}
		if _, dup := seen[v]; dup {
			return false
		}
		seen[v] = struct{}{}
		if v == cl.Primary {
			hasPrimary = true
		}
	}
	return hasPrimary // пустой Indices или Primary ∉ Indices → невалиден
}

// pendingTopic is one summarized+persisted topic awaiting the TOC delivery:
// its digests row id and the digest_items rows MarkDelivered will write.
type pendingTopic struct {
	digestID int64
	items    []storage.DigestItem
}

// Run executes one digest end-to-end — the 9-step delivery model of plan §C.
// Fatal: steps 1–2 (resolve/collect/filter) and 6–7 (all-topics-failed,
// delivery). Per-topic skip: step 5. Log-only: step 8 (§4.9 — a partial
// post-delivery failure never surfaces).
func (p *Pipeline) Run(ctx context.Context) error {
	log := p.log()

	// 1. Resolve the tracked sources for this run, wrap each in taggedSource
	// (stamps SourceID/TopicID onto a COPY of the collected items, §4.2) and
	// collect. Partial source errors are non-fatal (feed.Collect semantics).
	tracked, err := p.SourceProvider(ctx)
	if err != nil {
		return fmt.Errorf("resolve sources: %w", err)
	}
	sources := make([]feed.Source, 0, len(tracked))
	for _, t := range tracked {
		sources = append(sources, taggedSource{
			inner: t.Src, sourceID: t.SourceID, topicID: t.TopicID,
		})
	}
	items, collectErr := feed.Collect(ctx, sources)
	if collectErr != nil {
		log.Warn("partial source errors", "err", collectErr)
	}

	// 2. Drop items already delivered in a previous run; nothing new — exit
	// quietly (no TOC, no digests rows).
	fresh, err := p.Store.FilterUnseen(ctx, items)
	if err != nil {
		return fmt.Errorf("filter unseen: %w", err)
	}
	if len(fresh) == 0 {
		log.Info("no new items, skipping digest")
		return nil
	}

	// 3. Resolve topic names ONCE and group the fresh items by TopicID
	// (0 → 1 «Без темы»). Topics are ordered by id asc; items inside a topic
	// keep collect order (stable — no map iteration in the output).
	topics, err := p.Store.ListTopics(ctx)
	if err != nil {
		return fmt.Errorf("list topics: %w", err)
	}
	topicNames := make(map[int64]string, len(topics))
	for _, t := range topics {
		topicNames[t.ID] = t.Name
	}
	groups := make(map[int64][]feed.Item)
	var topicIDs []int64
	for _, it := range fresh {
		tid := it.TopicID
		if tid == 0 {
			tid = 1 // «Без темы»
		}
		if _, seen := groups[tid]; !seen {
			topicIDs = append(topicIDs, tid)
		}
		groups[tid] = append(groups[tid], it)
	}
	slices.Sort(topicIDs)

	// 3b. Вычитываем кеш обработки БАТЧЕМ по всем dedup_key (§4.8.1 шаг 3;
	// чанкование ≤500 ключей — внутри GetMessageStates). Строки done/dropped_*
	// не переспрашиваются у LLM: работа переживает крахи и рестарты (§4.9).
	keys := make([]string, 0, len(fresh))
	for _, it := range fresh {
		keys = append(keys, it.DedupKey())
	}
	states, err := p.Store.GetMessageStates(ctx, keys)
	if err != nil {
		return fmt.Errorf("get message states: %w", err)
	}

	// 4. One runID for the whole run (collision within a second is practically
	// impossible for a single-user service with LLM calls; tests fix Now).
	now := time.Now()
	if p.Now != nil {
		now = p.Now()
	}
	runID := now.Unix()

	// 5. Per topic (последовательно, §4.8.1 шаг 4): неклассифицированные
	// элементы → вызов A (ПОРЯДОК COLLECT — никаких map-итераций в промпт) →
	// кеш message_state ОДНИМ батчем → ad-сброс (seen до доставки) → render
	// L1 body → InsertDigest(delivered=0). A failed topic is SKIPPED: no
	// digests row, its items stay unseen and are picked up by the next run
	// (кеш message_state при этом сохраняется — §4.9).
	// linkFetchBudget — счётчик fetch'ей шага 4c, ОБЩИЙ НА ПРОГОН (§4.3:
	// ≤LinkFetchTotalMax GET'ов; темы обрабатываются последовательно —
	// гонки нет, указатель передаётся в processTopicLinks).
	linkFetchBudget := p.LinkFetchTotalMax
	var pending []pendingTopic
	entries := make([]TOCEntry, 0, len(topicIDs))
	headlines := make([]string, 0, len(topicIDs)) // параллельно entries — вход вызова D
	var droppedKeys []string                      // ad + персистентные дубли (seen ДО доставки)
	failed := 0
	for _, tid := range topicIDs {
		topicItems := groups[tid]
		name, ok := topicNames[tid]
		if !ok {
			// FK is not enforced — a dangling topic_id must not kill the run.
			name = fmt.Sprintf("Тема #%d", tid)
		}

		// 4a. Разделить элементы на закешированные (done/dropped_*) и те, что
		// требуют вызова A. Индексы unclsAt сохраняют порядок collect.
		cached := make(map[int]storage.MessageState, len(topicItems))
		var uncls []feed.Item
		var unclsAt []int
		for i, it := range topicItems {
			if st, hit := states[it.DedupKey()]; hit && isClassifiedState(st.Status) {
				cached[i] = st
				continue
			}
			uncls = append(uncls, it)
			unclsAt = append(unclsAt, i)
		}

		var res llm.ClassifyResult
		if len(uncls) > 0 {
			citems := make([]llm.ClassifyItem, len(uncls))
			for j, it := range uncls {
				citems[j] = llm.ClassifyItem{Title: it.Title, Text: it.Text}
			}
			res, err = p.Classify.ClassifySummarize(ctx, citems)
			if err != nil {
				// §4.9: сбой вызова A — тема пропускается ЦЕЛИКОМ, элементы
				// НЕ seen; Degraded (err==nil) тему не пропускает.
				// M1: при err результат может содержать ЧАСТИЧНЫЕ данные
				// (успешные чанки) — кэшируем не-Degraded часть ДО пропуска,
				// чтобы следующий прогон не переспрашивал LLM. Ошибка кеша
				// log-only: тема и так пропускается.
				if n := min(len(res.Items), len(uncls)); n > 0 {
					batch := make([]storage.MessageState, 0, n)
					for j := 0; j < n; j++ {
						if j < len(res.Degraded) && res.Degraded[j] {
							continue
						}
						batch = append(batch, messageStateFromClassify(uncls[j], tid, res.Items[j]))
					}
					if len(batch) > 0 {
						if uerr := p.Store.UpsertMessageState(ctx, batch); uerr != nil {
							log.Error("cache partial message_state failed",
								"topic_id", tid, "cached", len(batch), "err", uerr)
						}
					}
				}
				log.Error("classify failed; topic skipped",
					"topic_id", tid, "items", len(topicItems), "err", err)
				failed++
				continue
			}
			// L4: защита от нарушения контракта ClassifySummarizer (Items и
			// Degraded покрывают 1..n) — panic index-out-of-range в сервисе
			// недопустим; тема пропускается как сбой вызова A.
			if len(res.Items) != len(uncls) || len(res.Degraded) != len(uncls) {
				log.Error("classify result violates contract; topic skipped",
					"topic_id", tid, "items", len(res.Items),
					"degraded", len(res.Degraded), "want", len(uncls))
				failed++
				continue
			}
			// Кеш ОБЯЗАТЕЛЬНО до доставки (§4.8.1 шаг 4a: крах не теряет
			// работу): одна батч-транзакция на тему, ТОЛЬКО не-Degraded
			// элементы — деградации нечего кэшировать, следующий прогон
			// переспросит LLM (план строки 558–559).
			batch := make([]storage.MessageState, 0, len(uncls))
			for j, it := range uncls {
				if res.Degraded[j] {
					continue
				}
				batch = append(batch, messageStateFromClassify(it, tid, res.Items[j]))
			}
			if len(batch) > 0 {
				if err := p.Store.UpsertMessageState(ctx, batch); err != nil {
					log.Error("cache message_state failed; topic skipped",
						"topic_id", tid, "err", err)
					failed++
					continue
				}
				// L7: write-back в states — кросс-топик дубль dedup_key
				// (не-tg источники: ключ = чистый URL) в том же прогоне
				// берётся из кеша и повторно в LLM не уходит.
				for _, e := range batch {
					states[e.DedupKey] = e
				}
			}
		}

		// 4a-bis (Этап 9C, план §4.8.1 шаг 4a: вызов B ПОСЛЕ A): семантический
		// дедуп элементов, классифицированных A в ЭТОМ прогоне, class ∉ {ad},
		// в порядке collect. РЕШЕНИЕ ДИСПЕТЧЕРА (план §2 + тест-кейс 3):
		// закешированные (done/dropped_*) элементы в B НЕ участвуют —
		// кросс-прогонный семантический дедуп вне этапа; повторный Run делает
		// 0 вызовов B по закешированным. Ошибка B (транспорт или двойной
		// мусор) → деградация: ВСЕ элементы темы primary, тема НЕ
		// пропускается, Run продолжается (отличается от семантики ошибки A —
		// тест-кейс 1 плана 9C).
		//
		// Фикс 9C-ревью (решение диспетчера): IN-RUN исключение дублей
		// (dupDropped → kept-цикл) применяется для ВСЕХ валидных кластеров —
		// дедуп видим в дайджесте текущего прогона; но ПЕРСИСТ (строки B-батча
		// cluster_id/is_primary/dropped_dup) и seen (dup-ключи в потоке
		// MarkSeenBatch) — ТОЛЬКО для кластеров, где ВСЕ члены не-degraded.
		// Кластер с любым degraded-членом: строки не пишутся, seen не делается —
		// следующий прогон пересмотрит (симметрично §4.9 «деградация не
		// фиксируется навсегда»). Дубль при этом остаётся done/is_primary=1 от
		// A-батча и в следующем прогоне будет показан, если не перекластеризуется
		// — приемлемая деградация «дубль виден» вместо «элемент потерян».
		// Значение dupDropped: true → строка dropped_dup персистентна (нужен
		// seen), false → только in-run-исключение (degraded-кластер).
		dupDropped := make(map[int]bool) // индекс в topicItems → не-primary
		if p.Dedup != nil {
			var dItems []llm.DedupItem
			var dAt []int    // индекс в topicItems
			var dUncls []int // позиция в uncls
			for j := range uncls {
				if !res.Degraded[j] && res.Items[j].Class == llm.ClassAd {
					continue // реклама в кластеризацию не участвует (§4.4)
				}
				it := uncls[j]
				dItems = append(dItems, llm.DedupItem{
					Title: it.Title, Text: it.Text, SourceName: it.Source,
					Published: it.Published, SourceID: it.SourceID,
				})
				dAt = append(dAt, unclsAt[j])
				dUncls = append(dUncls, j)
			}
			if len(dItems) >= 2 {
				clusters, derr := p.Dedup.FindDuplicates(ctx, dItems)
				if derr != nil {
					log.Warn("dedup failed; topic kept without dedup",
						"topic_id", tid, "items", len(dItems), "err", derr)
				} else if len(clusters) > 0 {
					// L4: guard контракта Deduper — ДО любых обращений
					// Primary-1/Indices-1 (включая защитную ad-ветку ниже).
					// Невалидный кластер отбрасывается ЦЕЛИКОМ (log.Error, не
					// panic), тема продолжается с остальными кластерами.
					valid := make([]llm.DupCluster, 0, len(clusters))
					for _, cl := range clusters {
						if dupClusterValid(cl, len(dItems)) {
							valid = append(valid, cl)
							continue
						}
						log.Error("dedup cluster violates contract; cluster dropped",
							"topic_id", tid, "indices", cl.Indices, "primary", cl.Primary)
					}
					clusters = valid
				}
				if len(clusters) > 0 {
					// Защитная ветка (§4.4): primary, оказавшийся ad,
					// переназначается детерминированно. В 9C недостижима —
					// B идёт ПОСЛЕ A и ad исключены из входа; оставлена
					// планом (вызовы A и B независимы).
					for ci := range clusters {
						cl := &clusters[ci]
						pj := dUncls[cl.Primary-1]
						if !res.Degraded[pj] && res.Items[pj].Class == llm.ClassAd {
							cl.Primary = chooseDupPrimary(dItems, cl.Indices)
						}
					}
					// cluster_id = '<runID>:<n>', n — порядковый номер кластера
					// в валидированном ответе (1-based, §4.4). Второй
					// UpsertMessageState батч на тему ПОСЛЕ A-батча —
					// осознанное отклонение от буквального «A(+B) → один
					// Upsert» плана: крах между A и B не теряет классификацию
					// (инвариант «упавший прогон не теряет работу», 9A M1).
					batch := make([]storage.MessageState, 0, 2*len(clusters))
					for ci, cl := range clusters {
						cid := fmt.Sprintf("%d:%d", runID, ci+1)
						// Кластер персистится+seen ТОЛЬКО если ВСЕ его члены
						// не-degraded (фикс 9C-ревью): при любом degraded строки
						// не пишутся ВОВСЕ (в т.ч. для не-degraded дублей) —
						// иначе деградация зафиксируется навсегда (§4.9).
						// In-run-исключение (dupDropped) применяется в любом
						// случае; write-back ниже — только по записанным строкам.
						persist := true
						for _, idx := range cl.Indices {
							if res.Degraded[dUncls[idx-1]] {
								persist = false
								break
							}
						}
						for _, idx := range cl.Indices {
							j := dUncls[idx-1]
							isPrimary := idx == cl.Primary
							if !isPrimary {
								dupDropped[dAt[idx-1]] = persist
							}
							if !persist {
								continue // следующий прогон пересмотрит кластер
							}
							row := messageStateFromClassify(uncls[j], tid, res.Items[j])
							row.ClusterID = cid
							row.IsPrimary = isPrimary
							if !isPrimary {
								row.Status = stateDroppedDup // сразу seen (§4.4)
							}
							batch = append(batch, row)
						}
					}
					if len(batch) > 0 {
						if uerr := p.Store.UpsertMessageState(ctx, batch); uerr != nil {
							// Без персистентности dropped_dup помечать дубли seen
							// нельзя — деградация к «тема БЕЗ дедупа» (все
							// primary), как при ошибке B. Откат ЦЕЛИКОМ: in-run-
							// исключения degraded-кластеров тоже сбрасываются.
							log.Error("cache dedup message_state failed; topic kept without dedup",
								"topic_id", tid, "err", uerr)
							dupDropped = make(map[int]bool)
						} else {
							// states write-back (решение 9B/L7): только по
							// фактически записанным строкам — dup/primary-
							// изменения видны кросс-топик дублям dedup_key в
							// том же прогоне (поведение как у A-батча).
							for _, e := range batch {
								states[e.DedupKey] = e
							}
						}
					}
				}
			}
		}

		// 4b/4e. Собрать исход темы: ad → dropped (seen ДО доставки, в
		// digest_items не попадает); course → обычная строка с однофразным
		// summary из промпта A; остальные — полный цикл. Summary/class — из
		// кеша или из результатов вызова A (у Degraded — class=other и
		// extractLead-саммари из Items).
		pos := make(map[int]int, len(unclsAt))
		for j, i := range unclsAt {
			pos[i] = j
		}
		kept := make([]keptItem, 0, len(topicItems))
		for i, it := range topicItems {
			if st, hit := cached[i]; hit {
				if st.Status != stateDone { // dropped_ad / dropped_dup
					droppedKeys = append(droppedKeys, it.DedupKey())
					continue
				}
				kept = append(kept, keptItem{it: it, summary: st.Summary, class: st.Class})
				continue
			}
			if persisted, dup := dupDropped[i]; dup {
				// Non-primary по результату вызова B: in-run-исключение из
				// digest_items всегда (шаг 4c по ссылкам автоматически работает
				// только по kept), но seen ДО доставки (§4.4, тот же
				// MarkSeenBatch-поток, что и ad) — ТОЛЬКО если строка
				// dropped_dup персистентна: дубли degraded-кластера остаются
				// unseen, следующий прогон пересмотрит (фикс 9C-ревью).
				if persisted {
					droppedKeys = append(droppedKeys, it.DedupKey())
				}
				continue
			}
			r := res.Items[pos[i]]
			if !res.Degraded[pos[i]] && r.Class == llm.ClassAd {
				droppedKeys = append(droppedKeys, it.DedupKey())
				continue
			}
			class := r.Class
			if res.Degraded[pos[i]] {
				class = llm.ClassOther
			}
			kept = append(kept, keptItem{it: it, summary: r.Summary, class: class})
		}
		if len(kept) == 0 {
			// Тема целиком выброшена как реклама: нет строки digests и нет
			// записи в оглавлении. Это НЕ сбой темы (failed не растёт).
			continue
		}

		// 4c (Этап 9B, план §4.8.1 шаг 4 позиция c): внешние ссылки
		// kept-элементов — вызов E, fetch article/other (кеш link_cache),
		// вызов F; ↳-саммари конкатенируются в kept.summary и попадают ТОЛЬКО
		// в digest_items (message_state.summary остаётся базовым — повторный
		// прогон детерминированно пересобирает конкатенацию из link_cache).
		// Любая ошибка E/fetch/F — деградация (log.Warn), тема НЕ пропускается.
		p.processTopicLinks(ctx, kept, &linkFetchBudget)

		// 4d (Этап 9D, план §4.8.1 шаг 4 позиция d): вызов C — headline темы
		// + саммари топиков из саммари kept-элементов (primary-не-ad),
		// сгруппированных по source_id В ПОРЯДКЕ COLLECT (первое появление;
		// никакой map-итерации в промпт — §6, механика 9D строки 960–963).
		// Саммари — из message_state/вызова A (поле kept.summary; после шага
		// 4c оно включает ↳-добавки ссылок — план ставит 4d ПОСЛЕ 4c).
		// Ошибка транспорта → тема НЕ пропускается: fallback-headline
		// (первые ≤3 саммари списком «- …», паттерн offline-строки §4.7),
		// sections пусты (L1 деградирует на строки без саммари топиков).
		blocks := sourceBlocksFromKept(kept)
		var headline, sectionsJSON string
		if p.Aggregate != nil {
			agg, aerr := p.Aggregate.AggregateTopic(ctx, name, blocks)
			if aerr != nil {
				log.Error("topic aggregation failed; fallback headline",
					"topic_id", tid, "err", aerr)
				headline = fallbackHeadline(kept)
			} else {
				headline = agg.Headline
				sectionsJSON = topicSectionsJSON(blocks, agg.Sections)
			}
		}

		// Fallback titles (C1 отклонение 2): сохраняются для ПУСТОГО headline
		// (Aggregate не подключён или вернул пустую строку) — fallback-ветка
		// RenderTopicBody с буллетами первых заголовков.
		limit := min(len(kept), 10)
		fallbackTitles := make([]string, 0, limit)
		for _, k := range kept[:limit] {
			fallbackTitles = append(fallbackTitles, k.it.Title)
		}
		body := RenderTopicBody(name, headline, len(kept), now, fallbackTitles...)

		digestID, err := p.Store.InsertDigest(ctx, runID, tid, body, len(kept), sectionsJSON)
		if err != nil {
			log.Error("insert digest failed; topic skipped", "topic_id", tid, "err", err)
			failed++
			continue
		}
		ditems := make([]storage.DigestItem, 0, len(kept))
		for _, k := range kept {
			ditems = append(ditems, storage.NewDigestItem(k.it, k.summary, k.class))
		}
		pending = append(pending, pendingTopic{digestID: digestID, items: ditems})
		entries = append(entries, TOCEntry{
			DigestID: digestID, TopicName: name, ItemCount: len(kept),
		})
		headlines = append(headlines, headline)
	}

	// Реклама и персистентные дубли (dropped_dup) получают seen СРАЗУ
	// (§4.2/§4.9) — до доставки и независимо от неё: сброшенные элементы не
	// доставляются, «потерять» их нельзя. Ошибка log-only: непомеченный
	// следующий прогон повторно выбросит уже из кеша (без LLM).
	if len(droppedKeys) > 0 {
		if err := p.Store.MarkSeenBatch(ctx, droppedKeys); err != nil {
			log.Error("mark dropped seen failed", "dropped", len(droppedKeys), "err", err)
		}
	}

	// 6. Fresh items existed but EVERY topic failed → run error (§4.9). The
	// scheduler slot stays unmarked (runJob sets it only on success). Пустое
	// оглавление БЕЗ сбоев (всё отброшено как реклама) — тихий выход, не ошибка.
	if len(entries) == 0 {
		if failed == len(topicIDs) {
			return fmt.Errorf("digest: all %d topics failed", len(topicIDs))
		}
		if failed > 0 {
			// Смешанный сценарий: часть тем упала, часть отброшена как
			// реклама — невыполненное подберёт следующий прогон.
			log.Info("no digest entries", "failed_topics", failed,
				"note", "some topics failed; unseen items retry next run")
		} else {
			log.Info("all fresh items dropped as ads; no digest")
		}
		return nil
	}

	// 6 (Этап 9D, план §4.8.1 шаг 6): вызов D — общее саммари выпуска из
	// headline'ов тем (порядок = порядок entries), ОДИН на прогон, после всех
	// тем и ДО доставки. Сбой D (error ИЛИ пустой summary) — log-only:
	// overall="", TOC без общего саммари, Run продолжается (деградация, не
	// ошибка). РЕШЕНИЕ 9D-2b: строка runs.summary пишется ТОЛЬКО при непустом
	// summary — при сбое строки НЕТ (viewer TOC: нет строки → overall-блок
	// отсутствует; GetRunSummary-промах и пустая строка различимы в логах).
	var overall string
	if p.Overall != nil {
		ots := make([]llm.OverallTopic, 0, len(entries))
		for i, e := range entries {
			ots = append(ots, llm.OverallTopic{
				Name: e.TopicName, ItemCount: e.ItemCount, Headline: headlines[i],
			})
		}
		s, oerr := p.Overall.AggregateOverall(ctx, ots)
		switch {
		case oerr != nil:
			log.Error("overall aggregation failed; TOC without overall",
				"run_id", runID, "err", oerr)
		case strings.TrimSpace(s) == "":
			log.Warn("overall summary empty; TOC without overall", "run_id", runID)
		default:
			overall = s
			if uerr := p.Store.UpsertRunSummary(ctx, runID, overall); uerr != nil {
				log.Error("upsert run summary failed", "run_id", runID, "err", uerr)
			}
		}
	}

	// 7. Deliver ONE TOC message with per-topic «Открыть» buttons. Delivery
	// error → run error: all rows stay delivered=0, items unseen, slot
	// unmarked — the next run rebuilds the TOC.
	text := RenderTOC(entries, now.Format("02 Jan 2006"), overall)
	buttons := TOCButtons(entries)
	if err := p.Poster.DeliverWithButtons(ctx, p.ChatID, text, buttons); err != nil {
		return fmt.Errorf("deliver toc: %w", err)
	}

	// 8. Flip delivered=1 + write digest_items + mark seen, one transaction
	// per topic. Errors are log-only: the TOC already reached the user; an
	// affected topic's items stay unseen and duplicate into the next run
	// (accepted scenario, §4.9 / plan §C step 8).
	for _, pt := range pending {
		if err := p.Store.MarkDelivered(ctx, pt.digestID, pt.items); err != nil {
			log.Error("mark delivered failed", "digest_id", pt.digestID, "err", err)
		}
	}

	// 9.
	log.Info("digest delivered", "topics", len(entries), "run_id", runID)
	return nil
}

// TrackedSource couples a feed.Source with its storage identity (Stage 8 C,
// plan §4.2). SourceID 0 = no DB row (e.g. a static source that failed to
// upsert); TopicID 0 → «Без темы» (topic 1).
type TrackedSource struct {
	Src      feed.Source
	SourceID int64
	TopicID  int64
}

// taggedSource wraps a feed.Source and stamps SourceID/TopicID onto a COPY of
// the collected items (§4.2). The originals are never mutated: static sources
// are reused across runs. The inner error is returned as-is together with the
// (possibly partial) stamped items; NOTE that feed.Collect itself is
// sequential and DROPS a failed source's items (it appends the error and
// continues) — partial items are never consumed in the production path. No
// data is lost: undelivered items stay unseen and the next run picks them up.
type taggedSource struct {
	inner             feed.Source
	sourceID, topicID int64
}

// Name delegates to the inner source.
func (t taggedSource) Name() string { return t.inner.Name() }

// Collect stamps provenance onto a copy of the inner items.
func (t taggedSource) Collect(ctx context.Context) ([]feed.Item, error) {
	items, err := t.inner.Collect(ctx)
	stamped := make([]feed.Item, len(items))
	copy(stamped, items)
	for i := range stamped {
		stamped[i].SourceID = t.sourceID
		stamped[i].TopicID = t.topicID
	}
	return stamped, err
}

// sourceBlocksFromKept (шаг 4d, Этап 9D) группирует kept-элементы темы по
// source_id для входа вызова C. ДЕТЕРМИНИРОВАННО: блоки — в порядке ПЕРВОГО
// появления source_id в срезе kept (порядок collect), саммари внутри блока —
// в порядке collect; map используется только для точечного lookup, никогда
// для итерации в промпт (§6, механика 9D). Name — поле Source первого
// элемента группы (имя источника из collect).
func sourceBlocksFromKept(kept []keptItem) []llm.SourceBlock {
	pos := make(map[int64]int, len(kept))
	blocks := make([]llm.SourceBlock, 0, len(kept))
	for _, k := range kept {
		sid := k.it.SourceID
		if i, ok := pos[sid]; ok {
			blocks[i].Summaries = append(blocks[i].Summaries, k.summary)
			continue
		}
		pos[sid] = len(blocks)
		blocks = append(blocks, llm.SourceBlock{
			SourceID:  sid,
			Name:      k.it.Source,
			Summaries: []string{k.summary},
		})
	}
	return blocks
}

// fallbackHeadline — детерминированный fallback вызова C при ошибке
// транспорта (тест-кейс 8 подэтапа 9D): первые ≤3 непустых саммари темы
// списком «- …» в порядке collect — паттерн offline-строки §4.7 и
// llm.degradedTopicAggregate (двойной мусор). Пусто при отсутствии саммари —
// RenderTopicBody деградирует на буллеты заголовков.
func fallbackHeadline(kept []keptItem) string {
	const maxLines = 3 // aggregateFallbackLines в llm (§4.7: первые ≤3 строки)
	lines := make([]string, 0, maxLines)
	for _, k := range kept {
		if len(lines) >= maxLines {
			break
		}
		if s := strings.TrimSpace(k.summary); s != "" {
			lines = append(lines, "- "+s)
		}
	}
	return strings.Join(lines, "\n")
}

// topicSection — элемент digests.sections_json (миграция v6): формат
// [{"source_id":N,"summary":"…"}], совместимый с ParseTopicSections
// (контракт 9D-2a заморожен — порядок ключей в объекте не значим).
type topicSection struct {
	SourceID int64  `json:"source_id"`
	Summary  string `json:"summary"`
}

// topicSectionsJSON сериализует саммари топиков вызова C в sections_json.
// Порядок массива = порядок COLLECT (порядок blocks), а не порядок ответа
// LLM — контракт 9D-2a; дубль source_id → первый выигрыш, пустой summary →
// секции нет (паттерн parseAggregateTopic). Пустой результат → "" (пустая
// строка = «секций нет» для ParseTopicSections; [] не пишем).
func topicSectionsJSON(blocks []llm.SourceBlock, sections []llm.TopicHeadline) string {
	bySource := make(map[int64]string, len(sections))
	for _, s := range sections {
		sum := strings.TrimSpace(s.Summary)
		if sum == "" {
			continue
		}
		if _, dup := bySource[s.SourceID]; dup {
			continue
		}
		bySource[s.SourceID] = sum
	}
	out := make([]topicSection, 0, len(bySource))
	for _, b := range blocks {
		if sum, ok := bySource[b.SourceID]; ok {
			out = append(out, topicSection{SourceID: b.SourceID, Summary: sum})
		}
	}
	if len(out) == 0 {
		return ""
	}
	data, err := json.Marshal(out)
	if err != nil {
		return "" // недостижимо для string-полей; защитная деградация, не ошибка
	}
	return string(data)
}
