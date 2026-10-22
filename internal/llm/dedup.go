// Семантический дедуп (Этап 9C, вызов B плана §4.4/§4.8).
//
// Один вызов — батч на тему БЕЗ чанкования (гранулярность §4.8 «1 на тему»;
// план не предписывает чанки для B). Вход — пронумерованный список (заголовок
// + лид ≤200 рун + имя источника + source_id + дата), ответ строго JSON
// {"clusters":[{"indices":[1,4,7],"primary":4}]}. Критерий первоисточника
// задан В ПРОМПТЕ: самая ранняя публикация; при равенстве — самый полный
// текст; при равенстве — источник с меньшим source_id (детерминированный
// тай-брейк, §4.4).
//
// Парсер-валидатор — паттерн parseLinksResponse: fence-strip, ровно ОДИН
// повтор при мусоре, мусор после двух запросов → ERROR БЕЗ fallback (в
// отличие от вызова A): деградирует вызывающий — pipeline оставляет тему БЕЗ
// дедупа (все primary), а не пропускает её. Валидация: индексы вне 1..n →
// отброшены; пересечение между кластерами → ВТОРОЙ кластер отброшен целиком
// (первый выигрыш); primary ∉ indices → primary = первый индекс кластера;
// кластеры с <2 валидных индексов → отброшены (одиночные не требуют
// кластеризации). Пустой {"clusters":[]} — ВАЛИДНЫЙ ответ («дублей нет»),
// не мусор: решение 9A №8 про {"items":[]} здесь не применяется — отсутствие
// кластеров нормальный результат вызова B. Порядок — порядок ответа, никаких
// map-итераций в выходе.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// DedupItem — входной элемент вызова B. Pipeline строит срез из feed.Item
// (Title/Text/Source/Published/SourceID) по элементам темы, классифицированным
// вызовом A в ЭТОМ прогоне, class ∉ {ad}, в порядке collect (план §4.4:
// реклама в кластеризации не участвует; закешированные элементы не участвуют —
// кросс-прогонный семантический дедуп вне этапа, план §2).
type DedupItem struct {
	Title      string
	Text       string // лид; в промпте обрезается до dedupLeadRunes
	SourceName string
	Published  time.Time
	SourceID   int64
}

// DupCluster — кластер дублей: Indices — 1-based номера элементов в срезе,
// переданном FindDuplicates (≥2 валидных, порядок ответа), Primary — номер
// первоисточника (Primary ∈ Indices; гарантируется валидатором).
type DupCluster struct {
	Indices []int
	Primary int
}

// Deduper — вызов B плана §4.8: семантический дедуп, батч на тему. Реализуют
// и сетевой *Client, и *Offline (интерфейс на второй реализации — правило
// SKILL.md).
//
// Семантика ошибок: транспорт/API → error немедленно; мусорный ответ → ровно
// один повтор, затем error (БЕЗ fallback). Вызывающий (digest.Pipeline) при
// любой ошибке деградирует: тема остаётся БЕЗ дедупа (все элементы primary),
// тема НЕ пропускается, Run продолжается — в отличие от семантики ошибки
// вызова A (тест-кейс 1 плана 9C, решение планировщика).
// Пустой вход → пустой результат БЕЗ HTTP-запроса.
type Deduper interface {
	FindDuplicates(ctx context.Context, items []DedupItem) ([]DupCluster, error)
}

var (
	_ Deduper = (*Client)(nil)
	_ Deduper = (*Offline)(nil)
)

// dedupLeadRunes ограничивает заголовок и лид каждого элемента в промпте
// вызова B (план §4.4: лид ≤200 рун; title — тот же защитный бюджет, что в
// вызовах A/E, решение 9A №7).
const dedupLeadRunes = 200

const dedupSystemPrompt = `Ты — ассистент Telegram-бота, находящий дубликаты новостей в списке материалов дайджеста.
Ответь СТРОГО одним JSON-объектом вида {"clusters":[{"indices":[1,4,7],"primary":4}]} без пояснений и без markdown-обёрток.
Правила:
- clusters — только группы из двух и более материалов об одном и том же событии (переписанные новости считаются дубликатами);
- indices — порядковые номера материалов из списка (начиная с 1), по возрастанию; каждый номер входит не более чем в одну группу;
- primary — номер первоисточника в группе: самая ранняя дата публикации; при равенстве — самый полный текст; при равенстве — источник с меньшим source_id;
- если дубликатов нет — верни {"clusters":[]}.`

// buildDedupPrompt строит сообщения чата для вызова B — паттерн
// buildClassifyPrompt: детерминированно, порядок входа сохранён,
// нумерованные элементы (Title ≤200 рун + лид ≤200 рун + источник +
// source_id + дата). source_id и дата нужны LLM для тай-брейка первоисточника.
func buildDedupPrompt(items []DedupItem) []message {
	var b strings.Builder
	b.WriteString("Материалы:")
	if len(items) == 0 {
		b.WriteString("\n(недоступны)")
	}
	for i, it := range items {
		b.WriteString("\n")
		b.WriteString(itoa(i + 1))
		b.WriteString(". ")
		b.WriteString(truncateRunes(it.Title, dedupLeadRunes))
		if lead := truncateRunes(it.Text, dedupLeadRunes); lead != "" {
			b.WriteString("\n")
			b.WriteString(lead)
		}
		b.WriteString("\nисточник: ")
		b.WriteString(it.SourceName)
		b.WriteString(" (source_id=")
		b.WriteString(strconv.FormatInt(it.SourceID, 10))
		b.WriteString(")")
		if !it.Published.IsZero() {
			b.WriteString(", дата: ")
			b.WriteString(it.Published.UTC().Format(time.RFC3339))
		}
	}
	return []message{
		{Role: "system", Content: dedupSystemPrompt},
		{Role: "user", Content: b.String()},
	}
}

// parseDedupResponse разбирает ответ вызова B после stripCodeFence — паттерн
// parseLinksResponse. Валидация (план §4.4): индексы вне 1..n → отброшены;
// дубль индекса внутри кластера → первый выигрыш; пересечение между
// кластерами → второй кластер ОТБРОШЕН ЦЕЛИКОМ (первый выигрыш); кластеры с
// <2 валидных индексов → отброшены; primary ∉ indices → primary = первый
// индекс кластера. Выход детерминирован: порядок ответа LLM, никаких
// map-итераций. {"clusters":[]} (и даже все кластеры отброшены) → пустой
// результат БЕЗ ошибки: «дублей нет» — валидный исход, повтор не делается.
func parseDedupResponse(content string, n int) ([]DupCluster, error) {
	var resp struct {
		Clusters []struct {
			Indices []int `json:"indices"`
			Primary int   `json:"primary"`
		} `json:"clusters"`
	}
	if err := json.Unmarshal([]byte(stripCodeFence(content)), &resp); err != nil {
		return nil, fmt.Errorf("llm: parse dedup response: %w", err)
	}
	used := make([]bool, n+1) // индекс уже занят ВЫЖИВШИМ кластером
	var out []DupCluster
	for _, cl := range resp.Clusters {
		inCluster := make(map[int]struct{}, len(cl.Indices))
		idx := make([]int, 0, len(cl.Indices))
		crossed := false
		for _, v := range cl.Indices {
			if v < 1 || v > n {
				continue // индекс вне 1..n → отброшен
			}
			if _, dup := inCluster[v]; dup {
				continue // дубль внутри кластера — первый выигрыш
			}
			if used[v] {
				crossed = true // пересечение между кластерами
				break
			}
			inCluster[v] = struct{}{}
			idx = append(idx, v)
		}
		if crossed || len(idx) < 2 {
			continue // второй при пересечении отброшен ЦЕЛИКОМ; одиночные не кластеризуются
		}
		primary := cl.Primary
		if !slices.Contains(idx, primary) {
			primary = idx[0] // primary ∉ indices → первый индекс кластера
		}
		for _, v := range idx {
			used[v] = true
		}
		out = append(out, DupCluster{Indices: idx, Primary: primary})
	}
	return out, nil
}

// FindDuplicates implements Deduper (вызов B плана §4.8) over the
// chat-completions API — один запрос на тему без чанкования (гранулярность
// §4.8 «1 на тему»). Паттерн ClassifyLinks: строгий JSON, fence-strip, ровно
// ОДИН повтор при мусорном ответе (максимум 2 запроса), существующий
// retry/backoff транспорта (doWithRetry). Двойной мусор → ERROR без fallback
// (деградирует вызывающий: тема БЕЗ дедупа, не пропущена — §4.4). Ошибка
// транспорта/API → error немедленно. Пустой вход → nil, nil без HTTP-запроса.
// API-ключ никогда не логируется.
func (c *Client) FindDuplicates(ctx context.Context, items []DedupItem) ([]DupCluster, error) {
	if len(items) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(chatRequest{
		Model:       c.model,
		Messages:    buildDedupPrompt(items),
		Temperature: 0.2,
	})
	if err != nil {
		return nil, fmt.Errorf("llm: marshal request: %w", err)
	}

	url := c.baseURL + "/chat/completions"
	makeReq := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+c.apiKey) // apiKey never logged
		return req, nil
	}

	const maxParseAttempts = 2 // initial request + one repeat on an unparseable answer
	var lastErr error
	for attempt := 0; attempt < maxParseAttempts; attempt++ {
		resp, err := c.doWithRetry(ctx, makeReq)
		if err != nil {
			// HTTP-level failures were already retried inside doWithRetry;
			// fatal errors (400/401) must not be re-requested.
			return nil, err
		}
		content, err := extractContent(resp)
		if err != nil {
			return nil, err
		}
		clusters, perr := parseDedupResponse(content, len(items))
		if perr == nil {
			return clusters, nil
		}
		lastErr = perr
		c.log.Warn("llm dedup response unparseable", "attempt", attempt, "err", perr)
	}
	return nil, fmt.Errorf("llm: dedup unparseable after %d attempts: %w", maxParseAttempts, lastErr)
}

// offlineTitleTrimCutset — пунктуация, обрезаемая по краям в normalizeTitle
// (план 9C: «lowercase + trim пунктуации по краям + схлопывание пробелов»).
const offlineTitleTrimCutset = ".,;:!?…\"'`~*_-—–()[]{}<>«»“”‘’/|"

// normalizeTitle приводит заголовок к форме точного сравнения offline-дедупа
// (план 9C): lowercase → схлопывание пробелов → trim пунктуации по краям.
// Детерминированная чистая функция (без сети/random/time/map-итерации).
func normalizeTitle(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.Join(strings.Fields(s), " ")
	s = strings.Trim(s, offlineTitleTrimCutset)
	return strings.TrimSpace(s)
}

// FindDuplicates implements Deduper offline (план 9C): точное совпадение
// нормализованного заголовка В ПРЕДЕЛАХ ВХОДНОГО СРЕЗА — без сети. Совпало →
// кластер (порядок ответа — по первому вхождению группы, детерминированно),
// primary = первый в порядке входа (порядок collect); разные заголовки →
// кластеров нет. Элементы с пустым нормализованным заголовком не
// кластеризуются (иначе все безымянные посты слиплись бы в один «дубликат»).
// Два вызова на одном входе байт-в-байт одинаковы.
func (o *Offline) FindDuplicates(ctx context.Context, items []DedupItem) ([]DupCluster, error) {
	_ = ctx
	var groups []DupCluster
	groupAt := make(map[string]int, len(items)) // normalizeTitle → позиция в groups
	for i, it := range items {
		norm := normalizeTitle(it.Title)
		if norm == "" {
			continue
		}
		if g, ok := groupAt[norm]; ok {
			groups[g].Indices = append(groups[g].Indices, i+1)
			continue
		}
		groupAt[norm] = len(groups)
		groups = append(groups, DupCluster{Indices: []int{i + 1}, Primary: i + 1})
	}
	var out []DupCluster
	for _, cl := range groups {
		if len(cl.Indices) >= 2 { // одиночные не требуют кластеризации
			out = append(out, cl)
		}
	}
	return out, nil
}
