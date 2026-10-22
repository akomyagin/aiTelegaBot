// Этап 9B (план §4.3): извлечение внешних URL из сообщения перед вызовом E
// (классификация ссылок) и fetch. Оркестрация шага 4c pipeline — часть 4/4
// подэтапа 9B, здесь только детерминированное извлечение.

package digest

import (
	"context"
	"net"
	"net/url"
	"regexp"
	"strings"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
	"github.com/akomyagin/aiTelegaBot/internal/linkfetch"
	"github.com/akomyagin/aiTelegaBot/internal/llm"
	"github.com/akomyagin/aiTelegaBot/internal/storage"
)

// urlPattern — строго из плана §4.3: граница символа URL — пробел и <>"'.
// Компилируется один раз на пакет.
var urlPattern = regexp.MustCompile(`https?://[^\s<>"']+`)

// extractURLs извлекает внешние URL сообщения: Item.URL первым (если непустой
// и проходит фильтры), затем regex-скан Item.Text; нормализация; дедуп;
// порядок появления; ≤max. Возвращаются НОРМАЛИЗОВАННЫЕ URL — часть 4
// использует их как ключи link_cache и цели fetch. max ≤ 0 → пустой результат
// (защита). Ссылки сверх max просто отбрасываются здесь (план: «сверх лимита
// ссылки остаются в item_links» — это логика шага 4c, не извлечения).
//
// Нормализация (net/url):
//   - scheme и host — lowercase; path НЕ lowercased (case-sensitive);
//   - fragment (#…) отбрасывается;
//   - trailing "/" у path отбрасывается, ВКЛЮЧАЯ корневой: "http://h/" →
//     "http://h" (решение: единая форма, чтобы example.com/ и example.com
//     дедуплицировались в один ключ);
//   - query сохраняется байт-в-байт (RawQuery), escaped-path сохраняется
//     (EscapedPath) — строка собирается вручную, чтобы не переэкранировать;
//   - userinfo (user:pass@) отбрасывается ЦЕЛИКОМ (ревью F7) — креденшелы не
//     часть ключа кеша;
//   - непарсящиеся кандидаты и схемы кроме http/https отбрасываются.
//
// t.me/telegram.me НЕ считаются внешними (это «перейти» на сами каналы):
// отбрасываются host t.me и telegram.me с любыми поддоменами (решение
// диспетчера 9B-3).
func extractURLs(it feed.Item, max int) []string {
	if max <= 0 {
		return nil
	}
	var out []string
	seen := make(map[string]struct{})
	add := func(raw string) {
		if len(out) >= max {
			return
		}
		norm, ok := normalizeURL(raw)
		if !ok {
			return
		}
		if _, dup := seen[norm]; dup {
			return
		}
		seen[norm] = struct{}{}
		out = append(out, norm)
	}

	if it.URL != "" {
		add(it.URL)
	}
	for _, m := range urlPattern.FindAllString(it.Text, -1) {
		if len(out) >= max {
			break
		}
		add(m)
	}
	return out
}

// normalizeURL приводит кандидат к нормализованной форме. ok=false — кандидат
// отбрасывается (не http/https, не парсится, пустой host, telegram-хост).
func normalizeURL(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", false
	}
	host := strings.ToLower(u.Host)
	if host == "" {
		return "", false
	}
	if isTelegramHost(strings.ToLower(u.Hostname())) {
		return "", false
	}

	var b strings.Builder
	b.WriteString(u.Scheme) // url.Parse уже lowercасит scheme
	b.WriteString("://")
	// u.User отбрасывается ЦЕЛИКОМ (ревью F7): userinfo — не часть идентичности
	// статьи, а потенциальные креденшелы — им не место в ключе link_cache.
	b.WriteString(host)
	b.WriteString(strings.TrimRight(u.EscapedPath(), "/"))
	if u.RawQuery != "" {
		b.WriteByte('?')
		b.WriteString(u.RawQuery)
	}
	return b.String(), true
}

// isTelegramHost — host (без порта, lowercase) является t.me/telegram.me или
// их поддоменом.
func isTelegramHost(host string) bool {
	for _, domain := range []string{"t.me", "telegram.me"} {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

// LinkFetcher — инъекция linkfetch.Fetcher в pipeline. Интерфейс оправдан
// второй реализацией (правило SKILL.md §8): боевая *linkfetch.Fetcher и фейк
// в pipeline-тестах. Контракт: ровно один GET на URL (без ретраев); любая
// ошибка = страница недоступна (шаг 4c логирует в link_cache kind=unavailable).
type LinkFetcher interface {
	Fetch(ctx context.Context, url string) (linkfetch.Page, error)
}

var _ LinkFetcher = (*linkfetch.Fetcher)(nil)

// kindUnavailable — kind строки link_cache для страницы, которую не удалось
// скачать (§4.3: 4xx/429/503/не-HTML → unavailable, не ретраим — этика «≤1 GET»).
const kindUnavailable = "unavailable"

// linkDigestRunes — суммарный лимит summary элемента с ↳-добавками (Подэтап 9B:
// «≤600 рун суммарно», обрезка на границе руны — truncateRunes).
const linkDigestRunes = 600

// keptItem — элемент темы, доживший до рендера (шаги 4b/4e): исходный элемент
// + summary/class из кеша message_state или вызова A. Поле summary мутируется
// шагом 4c (processTopicLinks) — ТОЛЬКО ради digest_items; message_state уже
// закеширован на шаге 4a с базовым summary и не меняется.
type keptItem struct {
	it             feed.Item
	summary, class string
}

// linkCandidate — нормализованный URL-кандидат на fetch (kind ∈ {article,
// other} из вызова E): порядок collect/ord, дедуп по URL внутри темы.
type linkCandidate struct {
	url  string
	kind string
}

// processTopicLinks — оркестрация шага 4c (Этап 9B, план §4.3/§4.8.1 шаг 4
// позиция c) для ОДНОЙ темы: после kept-цикла 4b и ДО RenderTopicBody/
// InsertDigest — чтобы ↳-саммари попали в digest_items.
//
// Шаги: extractURLs (≤p.LinkPerMessageMax; только kept с class ∉ {ad, course})
// → ОДИН вызов E на тему → UpsertItemLinks (все извлечённые ссылки, Ord =
// 1-based порядок URL в сообщении) → ОДИН батч GetLinkCache по всем URL темы
// (кандидаты fetch + course/ad вместе) → course/ad получают строку link_cache
// БЕЗ fetch, но ТОЛЬКО если URL нет ни в прочитанном кеше, ни среди
// fetch-кандидатов темы (F1: ad-метка второго сообщения не затирает богатую
// строку и не конкурирует с fetch того же URL) → fetch-фаза (кеш-хит = строка
// kind ∈ {article, other, unavailable}; строка course/ad = ПРОМАХ с апгрейдом
// через DO UPDATE; SSRF-guard перед GET; budget ОБЩИЙ НА ПРОГОН; страница
// пишется в link_cache СРАЗУ после GET — крах не теряет скачанное) → вызов F
// батчем по свежим страницам → ↳-конкатенация: базовое summary + «\n↳ » +
// саммари каждой статьи kind ∈ {article, other} с НЕПУСТЫМ summary (свежим
// ИЛИ из кеша), суммарно ≤linkDigestRunes рун.
//
// Деградации (тема НЕ пропускается, дайджест цел):
//   - ошибка E → все пары kind=other в item_links, fetch и F НЕ выполняются
//     (решение диспетчера 9B-4: LLM-endpoint вероятно недоступен — F тоже
//     упадёт, а fetch без F навсегда залогирует пустое summary в бессрочном
//     кеше и потратит квоту «≤1 GET»);
//   - ошибка Fetch → строка link_cache kind=unavailable, без ретрая;
//   - бюджет исчерпан → оставшиеся ссылки живут в item_links с kind, без
//     строки кеша и без саммари;
//   - ошибка F → summary остаются пустыми;
//   - ошибки store — log-only; исключение UpsertItemLinks: без него шаг 4c
//     деградирует целиком (fetch-фаза не выполняется).
//
// Нил-сейвы: LinkClassify == nil → шаг пропускается ЦЕЛИКОМ (9A-совместимость);
// LinkFetch == nil → fetch-фаза пропускается (offline); LinkSummary == nil →
// фаза F пропускается.
//
// Детерминизм (§6): все обходы — срезы в порядке collect/ord; map используется
// только для точечных lookup (кеш, виды), никогда для итерации в промпты.
func (p *Pipeline) processTopicLinks(ctx context.Context, kept []keptItem, budget *int) {
	if p.LinkClassify == nil {
		return // шаг 4c не подключён
	}
	log := p.log()

	// 1. Collect: kept-элементы с class ∉ {ad, course} и их URL (порядок
	// collect сохраняется в entries).
	type msgEntry struct {
		keptIdx int      // индекс в kept
		urls    []string // нормализованные, ≤LinkPerMessageMax
	}
	var entries []msgEntry
	var msgs []llm.MsgLinks
	for i, k := range kept {
		if k.class == llm.ClassAd || k.class == llm.ClassCourse {
			continue
		}
		urls := extractURLs(k.it, p.LinkPerMessageMax)
		if len(urls) == 0 {
			continue
		}
		entries = append(entries, msgEntry{keptIdx: i, urls: urls})
		msgs = append(msgs, llm.MsgLinks{Text: k.it.Text, URLs: urls})
	}
	if len(msgs) == 0 {
		return
	}

	// 2. ОДИН вызов E на тему. Ошибка → все kind=other в item_links, БЕЗ fetch/F.
	kinds, err := p.LinkClassify.ClassifyLinks(ctx, msgs)
	if err != nil {
		log.Warn("link classify (E) failed; links degrade to other",
			"messages", len(msgs), "err", err)
		links := make([]storage.ItemLink, 0, len(msgs))
		for _, e := range entries {
			for ord, u := range e.urls {
				links = append(links, storage.ItemLink{
					DedupKey: kept[e.keptIdx].it.DedupKey(),
					URL:      u,
					Kind:     llm.LinkKindOther,
					Ord:      ord + 1,
				})
			}
		}
		if uerr := p.Store.UpsertItemLinks(ctx, links); uerr != nil {
			log.Error("upsert item_links failed (E-error degradation)", "err", uerr)
		}
		return
	}

	// Lookup видов: контракт E — ровно один LinkKind на пару (msg,link),
	// 1-based; отсутствующая/пустая пара → other (защита от нарушения
	// контракта — паттерн guard'ов 9A).
	kindByPair := make(map[[2]int]string, len(kinds))
	for _, lk := range kinds {
		kindByPair[[2]int{lk.Msg, lk.Link}] = lk.Kind
	}

	// 3. UpsertItemLinks — ВСЕ извлечённые ссылки (порядок записи — collect).
	links := make([]storage.ItemLink, 0, len(msgs))
	entryKinds := make([][]string, len(entries))
	for m, e := range entries {
		ks := make([]string, len(e.urls))
		for l, u := range e.urls {
			ks[l] = llm.LinkKindOther
			if k, ok := kindByPair[[2]int{m + 1, l + 1}]; ok && k != "" {
				ks[l] = k
			}
			links = append(links, storage.ItemLink{
				DedupKey: kept[e.keptIdx].it.DedupKey(),
				URL:      u,
				Kind:     ks[l],
				Ord:      l + 1,
			})
		}
		entryKinds[m] = ks
	}
	if err := p.Store.UpsertItemLinks(ctx, links); err != nil {
		// Без item_links повторный прогон не пересоберёт картину — шаг 4c
		// деградирует целиком (fetch не выполняем, дайджест цел).
		log.Error("upsert item_links failed; step 4c degraded",
			"links", len(links), "err", err)
		return
	}

	// 4. ОДИН батч GetLinkCache по ВСЕМ URL темы (кандидаты fetch И course/ad
	// вместе) — решение диспетчера F1: строка kind ∈ {course, ad} не должна ни
	// затирать богатый кеш, ни блокировать fetch. Внутри того же прогона вторая
	// тема видит строки, записанные immediate-upsert'ом первой (читаем БД).
	allURLs := make([]string, 0, len(links))
	seenURL := make(map[string]struct{}, len(links))
	for _, e := range entries {
		for _, u := range e.urls {
			if _, dup := seenURL[u]; dup {
				continue
			}
			seenURL[u] = struct{}{}
			allURLs = append(allURLs, u)
		}
	}
	cache, err := p.Store.GetLinkCache(ctx, allURLs)
	if err != nil {
		// Без состояния кеша fetch-фаза пропускается ЦЕЛИКОМ (вслепую легко
		// нарушить этику «≤1 GET»), и course/ad-строки НЕ пишутся: без фильтра
		// они могут затереть богатую строку. Ссылки остаются в item_links с
		// kind, без саммари (деградация).
		log.Error("get link_cache failed; fetch phase skipped",
			"urls", len(allURLs), "err", err)
		return
	}

	// 5a. Кандидаты fetch: kind ∈ {article, other}, дедуп по URL (порядок
	// collect).
	var cands []linkCandidate
	seenCand := make(map[string]struct{})
	for m, e := range entries {
		for l, u := range e.urls {
			k := entryKinds[m][l]
			if k == llm.LinkKindCourse || k == llm.LinkKindAd {
				continue
			}
			if _, dup := seenCand[u]; dup {
				continue
			}
			seenCand[u] = struct{}{}
			cands = append(cands, linkCandidate{url: u, kind: k})
		}
	}

	// 5b. Строки БЕЗ fetch для course/ad (§4.3: kind сохраняется, Title/Summary
	// пустые) — ТОЛЬКО для URL, которых нет в прочитанном кеше И нет в
	// множестве fetch-кандидатов (F1). Сценарий: msg1 X=article, msg2 X=ad в
	// одной теме → X фетчится один раз, ad-строка не пишется.
	var noFetchRows []storage.LinkCacheRow
	seenNoFetch := make(map[string]struct{})
	for m, e := range entries {
		for l, u := range e.urls {
			k := entryKinds[m][l]
			if k != llm.LinkKindCourse && k != llm.LinkKindAd {
				continue
			}
			if _, dup := seenNoFetch[u]; dup {
				continue
			}
			if _, inCache := cache[u]; inCache {
				continue
			}
			if _, isCand := seenCand[u]; isCand {
				continue
			}
			seenNoFetch[u] = struct{}{}
			noFetchRows = append(noFetchRows, storage.LinkCacheRow{URL: u, Kind: k})
		}
	}
	if len(noFetchRows) > 0 {
		if err := p.Store.UpsertLinkCache(ctx, noFetchRows); err != nil {
			log.Error("upsert link_cache (course/ad) failed",
				"rows", len(noFetchRows), "err", err)
		}
	}

	// summaries — непустые саммари статей по URL (свежие из F ИЛИ кеш-хиты);
	// только они дают ↳-добавку.
	summaries := make(map[string]string)
	if p.LinkFetch != nil && len(cands) > 0 {
		p.fetchLinkCandidates(ctx, cands, budget, cache, summaries)
	}

	// 7. ↳-конкатенация — ТОЛЬКО в kept.summary (уйдёт в NewDigestItem →
	// digest_items.summary); message_state.summary остаётся базовым.
	for m, e := range entries {
		base := kept[e.keptIdx].summary
		var b strings.Builder
		b.WriteString(base)
		changed := false
		for l, u := range e.urls {
			if k := entryKinds[m][l]; k != llm.LinkKindArticle && k != llm.LinkKindOther {
				continue
			}
			s := summaries[u]
			if s == "" {
				continue
			}
			b.WriteString("\n↳ ")
			b.WriteString(s)
			changed = true
		}
		if changed {
			kept[e.keptIdx].summary = truncateRunes(b.String(), linkDigestRunes)
		}
	}
}

// fetchLinkCandidates — fetch-фаза шага 4c + вызов F. cache прочитан
// вызывающим ОДНИМ батчем по всем URL темы (processTopicLinks шаг 4).
// Кеш-хит = строка kind ∈ {article, other, unavailable}: НЕ fetch'атся
// (непустое summary уходит в summaries; unavailable не ретраится никогда —
// план). Строка kind ∈ {course, ad} = ПРОМАХ (F1): сейчас E классифицировал
// ссылку как article/other — fetch апгрейдит строку через DO UPDATE (kind из
// текущего E, title из Page). Промахи — пока *budget > 0 (счётчик ОБЩИЙ НА
// ПРОГОН; каждая попытка Fetch его уменьшает, включая неудачную — GET уже
// потрачен). Перед Fetch цель проверяет SSRF-guard (F3): внутренний адрес
// пропускается БЕЗ траты бюджета и БЕЗ строки link_cache. Успех пишется в
// link_cache СРАЗУ после GET (крах не теряет скачанное, этика «≤1 GET»); любая
// ошибка Fetch → строка kind=unavailable БЕЗ ретрая. Бюджет исчерпан → ссылка
// остаётся в item_links с kind, без строки кеша и без саммари (деградация, не
// ошибка). Свежие страницы копятся для вызова F батчем; ошибка F → summary
// остаются пустыми.
func (p *Pipeline) fetchLinkCandidates(ctx context.Context, cands []linkCandidate,
	budget *int, cache map[string]storage.LinkCacheRow, summaries map[string]string) {
	log := p.log()

	var fresh []llm.LinkPage
	var freshKinds []string
	for _, c := range cands {
		if row, hit := cache[c.url]; hit &&
			row.Kind != llm.LinkKindCourse && row.Kind != llm.LinkKindAd {
			if row.Summary != "" {
				summaries[c.url] = row.Summary
			}
			continue
		}
		// SSRF-guard (F3) — до декремента бюджета: внутренний адрес не тратит
		// квоту «≤1 GET» и не попадает в link_cache.
		if blocked, why := isInternalFetchTarget(ctx, c.url); blocked {
			log.Warn("link fetch skipped: internal target (SSRF guard)",
				"url", c.url, "reason", why)
			continue
		}
		if *budget <= 0 {
			continue // бюджет прогона исчерпан — деградация без ошибки
		}
		*budget--
		page, err := p.LinkFetch.Fetch(ctx, c.url)
		if err != nil {
			log.Warn("link fetch failed; cached as unavailable",
				"url", c.url, "err", err)
			if uerr := p.Store.UpsertLinkCache(ctx, []storage.LinkCacheRow{
				{URL: c.url, Kind: kindUnavailable},
			}); uerr != nil {
				log.Error("upsert link_cache (unavailable) failed",
					"url", c.url, "err", uerr)
			}
			continue
		}
		// СРАЗУ после GET: kind+title уже ценны, summary дозапишет вызов F.
		if uerr := p.Store.UpsertLinkCache(ctx, []storage.LinkCacheRow{
			{URL: c.url, Kind: c.kind, Title: page.Title},
		}); uerr != nil {
			log.Error("upsert link_cache failed", "url", c.url, "err", uerr)
		}
		fresh = append(fresh, llm.LinkPage{URL: c.url, Title: page.Title, Text: page.Text})
		freshKinds = append(freshKinds, c.kind)
	}

	// 6. Вызов F батчем по свежим страницам; ошибка → summary пустые (§4.3).
	if len(fresh) == 0 || p.LinkSummary == nil {
		return
	}
	sums, err := p.LinkSummary.SummarizeLinks(ctx, fresh)
	if err != nil {
		log.Warn("link summarize (F) failed; summaries stay empty",
			"pages", len(fresh), "err", err)
		return
	}
	byIndex := make(map[int]string, len(sums))
	for _, s := range sums {
		byIndex[s.Index] = s.Summary
	}
	rows := make([]storage.LinkCacheRow, 0, len(fresh))
	for i := range fresh {
		s := byIndex[i+1] // Index 1-based; отсутствующий → "" (контракт F)
		rows = append(rows, storage.LinkCacheRow{
			URL: fresh[i].URL, Kind: freshKinds[i], Title: fresh[i].Title, Summary: s,
		})
		if s != "" {
			summaries[fresh[i].URL] = s
		}
	}
	if err := p.Store.UpsertLinkCache(ctx, rows); err != nil {
		log.Error("upsert link_cache summaries failed", "rows", len(rows), "err", err)
	}
}

// isInternalFetchTarget — SSRF-guard fetch-фазы (F3, решение диспетчера):
// бот живёт на VPS, и GET на внутренний адрес (loopback, приватные сети,
// link-local — включая cloud metadata 169.254.169.254) недопустим. Проверка
// живёт в digest-слое: linkfetch.Fetcher остаётся generic (его httptest-тесты
// на 127.0.0.1 не затрагиваются). Правила:
//   - literal IP (net.ParseIP): loopback/private/link-local unicast/link-local
//     multicast/unspecified → блок;
//   - hostname "localhost" (без учёта регистра) → блок;
//   - иначе best-effort net.DefaultResolver.LookupIPAddr(ctx, host) и та же
//     проверка резолвленных IP; ошибка резолва НЕ блокирует (fail-open:
//     реальный GET обработает Fetch со своей политикой ошибок/таймаутов).
//
// Guard — best-effort: DNS-rebinding (TOCTOU между проверкой и GET) вне скоупа
// — приемлемый риск для однопользовательского pet-проекта (заметка техдолга).
func isInternalFetchTarget(ctx context.Context, raw string) (bool, string) {
	u, err := url.Parse(raw)
	if err != nil {
		return false, "" // нормализованный URL сюда не попадёт; оставляем Fetch
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true, "localhost"
	}
	if ip := net.ParseIP(host); ip != nil {
		if isInternalIP(ip) {
			return true, "literal internal IP"
		}
		return false, ""
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return false, "" // сбой резолва не блокирует — обработает Fetch
	}
	for _, a := range addrs {
		if isInternalIP(a.IP) {
			return true, "resolves to internal IP"
		}
	}
	return false, ""
}

// isInternalIP — IP, fetch на который блокируется (F3).
func isInternalIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}
