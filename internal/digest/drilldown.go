// Диспетчер четырёхуровневой навигации (Этап 9D-2a, план §4.6 и «Подэтап 9D»):
// seam telegram.WithDrillDown разбирает СЫРОЙ callback_data —
// «S:<digestID>:<sourceID>» (топик, sourceID=0 → все топики одним списком с
// подзаголовками), «M:<digestItemID>» (сообщение), «P:<digestItemID>» (фото —
// Этап 9E: CallbackResult{Send} → sendPhoto в owner-чат) и «PDEL» (закрыть
// фото — в проде перехватывает telegram-диспетчер; здесь защита прямых
// вызовов, ack-only), ПЛЮС legacy-alias'ы кнопок СТАРЫХ
// доставленных сообщений: «L2:<digestID>» → как «T:<digestID>» (ближайший
// новый аналог — RenderTopicView), «L3:<digestID>:<sourceID>» →
// «S:<digestID>:<sourceID>» (включая sourceID=0). «L1:» в seam не попадает —
// это alias T:-пути в telegram-диспетчере. Данные строго из SQLite
// (rest-семантика §4.3): view переживают рестарт бота — саммари топиков
// читаются из digests.sections_json (миграция v6, директива диспетчера).
// Ошибка seam'а (мусорные data, неизвестный id, сбой storage) → error:
// диспетчер telegram сам ack'ит «Не получилось открыть» и логирует, паник
// здесь не должно быть.

package digest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/akomyagin/aiTelegaBot/internal/storage"
	"github.com/akomyagin/aiTelegaBot/internal/telegram"
)

const (
	// drillBackLabel — кнопка «◀ Назад» (используется и render_nav.go).
	drillBackLabel = "◀ Назад"
	// Этап 9E (план §4.5.2): фото-сообщение «P:» — лейбл «✖ Закрыть»
	// (callback_data «PDEL» = 4 байта ≤ 64) и рунные верхние капы компонентов
	// caption (первая ступень ДО байтового ужатия). Caption —
	// «<b>title</b>\n<summary>\n↪ <a href>источник</a>», жёсткий лимит Bot API
	// 1024 БАЙТА — telegram.PhotoCaptionMaxBytes (единственная константа,
	// ревью 9E фикс 8); гарантируется НЕ финальной байтовой резкой готового
	// HTML (рвёт сущности/теги), а ужатием компонентов ДО сборки
	// (photoCaption): EscapeHTML РАСТЯГИВАЕТ текст (& → &amp;, до 5x),
	// поэтому бюджет считается по байтам ЭСКЕЙПНУТОГО фрагмента.
	photoCloseLabel          = "✖ Закрыть"
	photoDeleteCallbackData  = "PDEL"
	photoCaptionTitleRunes   = 120
	photoCaptionSummaryRunes = 200
)

// parseSourceData разбирает «<prefix><digestID>:<sourceID>» для «S:» и legacy
// «L3:» (alias §4.6): digestID ≥ 1, sourceID ≥ 0 (0 = все топики). Мусор,
// не-числа, лишние сегменты и переполнение int64 → error, не паника
// (strconv.ParseInt отвергает всё сам).
func parseSourceData(data, prefix string) (digestID, sourceID int64, err error) {
	rest, ok := strings.CutPrefix(data, prefix)
	if !ok {
		return 0, 0, fmt.Errorf("drill-down: data %q: want prefix %q", data, prefix)
	}
	didStr, sidStr, found := strings.Cut(rest, ":")
	if !found {
		return 0, 0, fmt.Errorf("drill-down: bad %s data %q: want <digestID>:<sourceID>", prefix, data)
	}
	did, idErr := parseIDBounded(didStr, 1)
	if idErr != nil {
		return 0, 0, fmt.Errorf("drill-down: bad %s digest id %q: %w", prefix, data, idErr)
	}
	sid, idErr := parseIDBounded(sidStr, 0)
	if idErr != nil {
		return 0, 0, fmt.Errorf("drill-down: bad %s source id %q: %w", prefix, data, idErr)
	}
	return did, sid, nil
}

// parseMessageData разбирает «<prefix><digestItemID>» для «M:», «P:» и legacy
// «L2:» (один числовой аргумент ≥ 1). Мусор/overflow → error, не паника.
func parseMessageData(data, prefix string) (int64, error) {
	rest, ok := strings.CutPrefix(data, prefix)
	if !ok {
		return 0, fmt.Errorf("drill-down: data %q: want prefix %q", data, prefix)
	}
	id, err := parseIDBounded(rest, 1)
	if err != nil {
		return 0, fmt.Errorf("drill-down: bad %s id %q: %w", prefix, data, err)
	}
	return id, nil
}

// parseIDBounded парсит десятичный id и требует value >= min (digestID/
// itemID ≥ 1, sourceID ≥ 0). ParseInt сам отвергает мусор, «», переполнение
// int64 и значения ниже min.
func parseIDBounded(s string, min int64) (int64, error) {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, err
	}
	if v < min {
		return 0, fmt.Errorf("id %d below min %d", v, min)
	}
	return v, nil
}

// NewDrillDown собирает seam для telegram.WithDrillDown (план §4.6, Этап 9D):
// диспетчер S:/M:/P:/PDEL + legacy-alias'ы L2:/L3:. data приходит СЫРЫМ с
// префиксом — парсинг здесь. sources — снапшот строк sources для имён топиков
// (в app: store.ListSources, один вызов на view, кэш не нужен — single-user,
// таблица маленькая); отображаемое имя = Ref c peer-Title fallback для tg_*
// (SourceDisplayNames, план §4.6 строки 393–397).
func NewDrillDown(store storage.Store, sources func(ctx context.Context) ([]storage.Source, error)) telegram.DrillDown {
	return func(ctx context.Context, data string) (*telegram.CallbackResult, error) {
		switch {
		case strings.HasPrefix(data, "S:"):
			did, sid, err := parseSourceData(data, "S:")
			if err != nil {
				return nil, err
			}
			return drillSourceView(ctx, store, sources, did, sid)
		case strings.HasPrefix(data, "M:"):
			id, err := parseMessageData(data, "M:")
			if err != nil {
				return nil, err
			}
			return drillMessageView(ctx, store, id)
		case strings.HasPrefix(data, "P:"):
			// Этап 9E (план §4.5.2): фото материала — отдельное sendPhoto-
			// сообщение (CallbackResult.Send); 9D-заглушка «Медиа недоступно»
			// удалена (осознанная смена контракта 9E).
			id, err := parseMessageData(data, "P:")
			if err != nil {
				return nil, err
			}
			return drillPhotoSend(ctx, store, id)
		case data == photoDeleteCallbackData:
			// Защита прямых вызовов seam'а (как в 9D): в проде «PDEL»
			// перехватывает telegram-диспетчер РАНЬШЕ seam'а (handlePhotoDelete:
			// deleteMessage того же сообщения + ack, аргументов нет) — эта
			// ветка недостижима и остаётся ack-only.
			return &telegram.CallbackResult{}, nil
		case strings.HasPrefix(data, "L2:"):
			// Legacy alias §4.6: «L2:<did>» → как «T:<did>» (ближайший новый
			// аналог «по источникам» — L1-view темы).
			did, err := parseMessageData(data, "L2:")
			if err != nil {
				return nil, err
			}
			return drillTopicView(ctx, store, sources, did)
		case strings.HasPrefix(data, "L3:"):
			// Legacy alias §4.6: «L3:<did>:<sid>» → «S:<did>:<sid>».
			did, sid, err := parseSourceData(data, "L3:")
			if err != nil {
				return nil, err
			}
			return drillSourceView(ctx, store, sources, did, sid)
		}
		return nil, fmt.Errorf("drill-down: unknown callback data %q", data)
	}
}

// drillTopicView — L1-view темы (T:-аналог для legacy «L2:<did>» и основа
// viewer'а): шапка (digests.body как есть) + топики именами с саммари из
// sections_json + кнопки S:<did>:<sid> и «◀ К оглавлению».
func drillTopicView(ctx context.Context, store storage.Store, sources func(context.Context) ([]storage.Source, error), digestID int64) (*telegram.CallbackResult, error) {
	d, ok, err := store.GetDigest(ctx, digestID)
	if err != nil {
		return nil, fmt.Errorf("drill-down: GetDigest(%d): %w", digestID, err)
	}
	if !ok {
		// Crafted/устаревший id — error (ack+лог диспетчера), НЕ паника.
		return nil, fmt.Errorf("drill-down: digest %d не существует", digestID)
	}
	items, err := store.ListDigestItems(ctx, digestID)
	if err != nil {
		return nil, fmt.Errorf("drill-down: ListDigestItems(%d): %w", digestID, err)
	}
	names, err := SourceDisplayNames(ctx, store, sources)
	if err != nil {
		return nil, err
	}
	nav := RenderTopicView(d.Body, digestID, d.RunID,
		TopicEntries(items, names, ParseTopicSections(d.SectionsJSON)))
	return navViewResult(nav), nil
}

// drillSourceView — L2-view топика («S:<did>:<sid>»; sid=0 → все топики с
// подзаголовками): шапка «📰 <тема> / <имя топика>» + саммари топика из
// sections_json + блоки сообщений с «перейти» и кнопками «Читать ▸» M:<itemID>.
func drillSourceView(ctx context.Context, store storage.Store, sources func(context.Context) ([]storage.Source, error), digestID, sourceID int64) (*telegram.CallbackResult, error) {
	d, ok, err := store.GetDigest(ctx, digestID)
	if err != nil {
		return nil, fmt.Errorf("drill-down: GetDigest(%d): %w", digestID, err)
	}
	if !ok {
		return nil, fmt.Errorf("drill-down: digest %d не существует", digestID)
	}
	topicName, err := drillTopicName(ctx, store, d.TopicID)
	if err != nil {
		return nil, err
	}
	var items []storage.DigestItem
	if sourceID == 0 {
		items, err = store.ListDigestItems(ctx, digestID)
	} else {
		items, err = store.ListDigestItemsBySource(ctx, digestID, sourceID)
	}
	if err != nil {
		return nil, fmt.Errorf("drill-down: items(%d,%d): %w", digestID, sourceID, err)
	}
	names, err := SourceDisplayNames(ctx, store, sources)
	if err != nil {
		return nil, err
	}
	sections := SourceSections(items, names, ParseTopicSections(d.SectionsJSON))
	return navViewResult(RenderSourceView(topicName, digestID, sourceID, sections)), nil
}

// drillMessageView — L3-view сообщения («M:<itemID>», план §4.6): full_text
// (пуст → snippet + пометка legacy), блок «Ссылки:» из item_links+link_cache,
// кнопки «🖼 Фото» P: (при media_ref≠”), «↗ Перейти в источник» (URL-кнопка)
// и «◀ Назад» S:<did>:<sid> (did/sid из самой строки digest_items).
func drillMessageView(ctx context.Context, store storage.Store, itemID int64) (*telegram.CallbackResult, error) {
	it, ok, err := store.GetDigestItem(ctx, itemID)
	if err != nil {
		return nil, fmt.Errorf("drill-down: GetDigestItem(%d): %w", itemID, err)
	}
	if !ok {
		return nil, fmt.Errorf("drill-down: digest item %d не существует", itemID)
	}
	links, err := messageLinks(ctx, store, it.DedupKey)
	if err != nil {
		return nil, err
	}
	return navViewResult(RenderMessageView(it, links)), nil
}

// messageLinks — блок «Ссылки:» L3-view: item_links (порядок ord ASC) +
// title/summary из link_cache (9B). Пропавшая cache-строка → Title/Summary
// пусты (рендер деградирует на домен, не ошибка). Kind — из item_links, при
// пустом — из кеша.
func messageLinks(ctx context.Context, store storage.Store, dedupKey string) ([]MessageLink, error) {
	rows, err := store.ListItemLinks(ctx, dedupKey)
	if err != nil {
		return nil, fmt.Errorf("drill-down: ListItemLinks(%q): %w", dedupKey, err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	urls := make([]string, 0, len(rows))
	for _, r := range rows {
		urls = append(urls, r.URL)
	}
	cache, err := store.GetLinkCache(ctx, urls)
	if err != nil {
		return nil, fmt.Errorf("drill-down: GetLinkCache: %w", err)
	}
	out := make([]MessageLink, 0, len(rows))
	for _, r := range rows {
		l := MessageLink{URL: r.URL, Kind: r.Kind}
		if c, ok := cache[r.URL]; ok {
			if l.Kind == "" {
				l.Kind = c.Kind
			}
			l.Title, l.Summary = c.Title, c.Summary
		}
		out = append(out, l)
	}
	return out, nil
}

// NavButtons собирает итоговые строки клавиатуры NavView (контракт d1.md):
// i-я nil-строка Buttons — плейсхолдер URLButtons[i] → строка
// {InlineButton{Text, URL}} (InlineButton.URL добавлен в 9D-2a). Инвариант
// «число nil-строк == len(URLButtons)» зафиксирован тестом 9D-1; нарушение
// инварианта здесь — пропуск плейсхолдера (защита, не паника).
func NavButtons(v *NavView) [][]telegram.InlineButton {
	if v == nil {
		return nil
	}
	rows := make([][]telegram.InlineButton, 0, len(v.Buttons))
	next := 0
	for _, row := range v.Buttons {
		if row == nil {
			if next >= len(v.URLButtons) {
				continue
			}
			ub := v.URLButtons[next]
			next++
			row = []telegram.InlineButton{{Text: ub.Text, URL: ub.URL}}
		}
		rows = append(rows, row)
	}
	return rows
}

// navViewResult конвертирует NavView в CallbackResult: ack пустой, Edit —
// текст + клавиатура с подставленными URL-кнопками (NavButtons).
func navViewResult(v *NavView) *telegram.CallbackResult {
	if v == nil {
		return &telegram.CallbackResult{}
	}
	return &telegram.CallbackResult{Edit: &telegram.EditMessage{
		Text:    v.Text,
		Buttons: NavButtons(v),
	}}
}

// TopicSection — элемент digests.sections_json (миграция v6): саммари топика
// из вызова C; порядок массива = порядок collect.
type TopicSection struct {
	SourceID int64  `json:"source_id"`
	Summary  string `json:"summary"`
}

// ParseTopicSections разбирает digests.sections_json в map source_id → саммари
// топика. Пустая строка (legacy/v5-строки) или битый JSON → пустая map:
// L1/L2 деградируют на строки без саммари (план §4.6), НЕ ошибка. Дубль
// source_id — первый выигрыш; пустой summary → секции нет (стиль 9D-1 §C).
func ParseTopicSections(sectionsJSON string) map[int64]string {
	out := make(map[int64]string)
	if strings.TrimSpace(sectionsJSON) == "" {
		return out
	}
	var arr []TopicSection
	if err := json.Unmarshal([]byte(sectionsJSON), &arr); err != nil {
		return make(map[int64]string)
	}
	for _, s := range arr {
		if _, dup := out[s.SourceID]; dup {
			continue
		}
		if strings.TrimSpace(s.Summary) == "" {
			continue
		}
		out[s.SourceID] = s.Summary
	}
	return out
}

// SourceDisplayNames — снапшот id → отображаемое имя источника (план §4.6,
// строки 393–397): имя = Ref (RSS URL / username канала); для tg_*-источников
// с Ref в форме «-100X» — Title из mtproto_peers (store.GetPeer), если строка
// есть и Title непустой. Ошибка/промах GetPeer деградирует в Ref (кеш-таблица
// не должна ронять view). Публичный username-Ref остаётся как есть: GetPeer
// keyed by id, запроса по username в схеме нет (осознанное ограничение 9D-2a).
// sources == nil → пустой снапшот (имена деградируют до «источник #N»).
func SourceDisplayNames(ctx context.Context, store storage.Store, sources func(context.Context) ([]storage.Source, error)) (map[int64]string, error) {
	names := make(map[int64]string)
	if sources == nil {
		// Защитная ветка (app всегда передаёт снапшот): все имена деградируют
		// до «источник #N», view остаётся работоспособным.
		return names, nil
	}
	rows, err := sources(ctx)
	if err != nil {
		return nil, fmt.Errorf("drill-down: sources snapshot: %w", err)
	}
	for _, s := range rows {
		names[s.ID] = sourceDisplayName(ctx, store, s)
	}
	return names, nil
}

// sourceDisplayName — имя одного источника с peer-Title fallback (см.
// SourceDisplayNames).
func sourceDisplayName(ctx context.Context, store storage.Store, s storage.Source) string {
	if store != nil && strings.HasPrefix(s.Kind, "tg_") {
		if rest, ok := strings.CutPrefix(s.Ref, "-100"); ok {
			if x, err := strconv.ParseInt(rest, 10, 64); err == nil && x > 0 {
				if p, found, err := store.GetPeer(ctx, x); err == nil && found && strings.TrimSpace(p.Title) != "" {
					return p.Title
				}
			}
		}
	}
	return s.Ref
}

// TopicEntries группирует digest_items в строки L1-view (порядок = id ASC,
// первое появление источника задаёт строку — детерминированный рендер, никаких
// map-итераций). Summary — саммари топика из sections_json (пусто → строка без
// саммари, деградация плана).
func TopicEntries(items []storage.DigestItem, names map[int64]string, sections map[int64]string) []TopicEntry {
	idx := make(map[int64]int, len(items))
	var out []TopicEntry
	for _, it := range items {
		if i, ok := idx[it.SourceID]; ok {
			out[i].ItemCount++
			continue
		}
		idx[it.SourceID] = len(out)
		out = append(out, TopicEntry{
			SourceID:  it.SourceID,
			Name:      names[it.SourceID],
			ItemCount: 1,
			Summary:   sections[it.SourceID],
		})
	}
	return out
}

// SourceSections группирует digest_items в секции L2-view (тот же порядок
// первого появления); Summary — саммари топика из sections_json.
func SourceSections(items []storage.DigestItem, names map[int64]string, sections map[int64]string) []SourceSection {
	idx := make(map[int64]int, len(items))
	var out []SourceSection
	for _, it := range items {
		if i, ok := idx[it.SourceID]; ok {
			out[i].Items = append(out[i].Items, it)
			continue
		}
		idx[it.SourceID] = len(out)
		out = append(out, SourceSection{
			SourceID: it.SourceID,
			Name:     names[it.SourceID],
			Summary:  sections[it.SourceID],
			Items:    []storage.DigestItem{it},
		})
	}
	return out
}

// drillTopicName — имя темы из ListTopics; нет строки → «Тема #<id>»
// (паттерн C2, app/digest_nav.go; FK не enforcement'ится).
func drillTopicName(ctx context.Context, store storage.Store, topicID int64) (string, error) {
	topics, err := store.ListTopics(ctx)
	if err != nil {
		return "", fmt.Errorf("drill-down: ListTopics: %w", err)
	}
	for _, t := range topics {
		if t.ID == topicID {
			return t.Name, nil
		}
	}
	return "Тема #" + strconv.FormatInt(topicID, 10), nil
}

// l3Link — ссылка элемента для <a href> (план §D + §4.8): url != "" И
// схема http/https (проверяется ПЕРЕД вставкой; url.Parse нормализует
// схему в нижний регистр и отвергает управляющие символы) → url; иначе
// telegram.DeepLink(tg_peer, tg_msg_id); оба пусты/биты → "" (без <a>).
// Кавычка в url отвергается — она сломала бы href-атрибут; EscapeHTML
// экранирует & < > в готовом URL.
func l3Link(it storage.DigestItem) string {
	if it.URL != "" && !strings.Contains(it.URL, `"`) {
		if u, err := url.Parse(it.URL); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
			return telegram.EscapeHTML(it.URL)
		}
	}
	return telegram.DeepLink(it.TgPeer, it.TgMsgID)
}

// fitDrillDownList — локальный guard 4096 рун для view навигации (паттерн
// старого RenderTopicL1, план §D «обрезка с хвоста с пометкой “…и ещё K”»).
// Хвостовые блоки отбрасываются по одному, K = число отброшенных БЛОКОВ.
// Полоса M1 (ревью C1): цикл не тестирует «заголовок allein» — если
// заголовок ≤4096 рун, он возвращается как есть; длиннее — режется по рунам
// с «…».
func fitDrillDownList(header string, blocks []string, sep string) string {
	for dropped := 0; dropped <= len(blocks); dropped++ {
		block := strings.Join(blocks[:len(blocks)-dropped], sep)
		if dropped > 0 {
			marker := "…и ещё " + strconv.Itoa(dropped)
			if block != "" {
				block += "\n" + marker
			} else {
				block = marker
			}
		}
		full := header
		if block != "" {
			full = header + "\n\n" + block
		}
		if utf8.RuneCountInString(full) <= maxBodyRunes {
			return full
		}
	}
	r := []rune(header)
	if len(r) <= maxBodyRunes {
		return header
	}
	return string(r[:maxBodyRunes-1]) + "…"
}

// drillPhotoSend — «P:<itemID>» (Этап 9E, план §4.5.2): тот же storage-запрос,
// что у M:-view (GetDigestItem), media_ref=” (или item не найден) → error —
// telegram-диспетчер ack'ит «Не получилось открыть» (существующая семантика
// ошибки DrillDown; «🖼 Фото» без media_ref не рендерится, но crafted-callback
// и гонка с миграцией возможны). Иначе — CallbackResult{Send}: sendPhoto в
// owner-чат (сторона telegram), caption «<b>title</b>\n<summary>\n↪ <a
// href="LINK">источник</a>» (≤1024 байт), клавиатура — «✖ Закрыть» PDEL.
func drillPhotoSend(ctx context.Context, store storage.Store, itemID int64) (*telegram.CallbackResult, error) {
	it, ok, err := store.GetDigestItem(ctx, itemID)
	if err != nil {
		return nil, fmt.Errorf("drill-down: GetDigestItem(%d): %w", itemID, err)
	}
	if !ok {
		return nil, fmt.Errorf("drill-down: digest item %d не существует", itemID)
	}
	if strings.TrimSpace(it.MediaRef) == "" {
		return nil, fmt.Errorf("drill-down: digest item %d: media_ref пуст", itemID)
	}
	return &telegram.CallbackResult{Send: &telegram.SendAction{
		PhotoMediaRef: it.MediaRef,
		PhotoCaption:  photoCaption(it),
		PhotoButtons: [][]telegram.InlineButton{
			{{Text: photoCloseLabel, Data: photoDeleteCallbackData}},
		},
	}}, nil
}

// photoCaption — caption фото-сообщения (план §4.5.2/9E): «<b>title</b>\n
// <summary ≤200 рун>\n↪ <a href="LINK">источник</a>». ГАРАНТИЯ (ревью 9E,
// фикс 1): результат ВСЕГДА well-formed HTML ≤1024 байт
// (telegram.PhotoCaptionMaxBytes) — компоненты ужимаются КАК PLAIN TEXT ДО
// сборки и только потом EscapeHTML; байтовая резка готового HTML (старый
// truncateToBytes) рвала «<a href="…» и сущности → Telegram 400 «can't parse
// entities». href — l3Link (только http/https, кавычка отвергнута, fallback
// t.me deep link; пуст → строка «↪» не рендерится). Приоритет ужатия при
// переполнении: (1) summary, (2) title — оба по байтам ЭСКЕЙПНУТОГО
// фрагмента с «…» на границе рун, (3) ОТБРОСИТЬ строку «↪ <a …>источник</a>»
// ЦЕЛИКОМ (обрезанный URL — битая ссылка) с пережатием title/summary в
// освободившийся бюджет. Порядок компонентов не меняется.
func photoCaption(it storage.DigestItem) string {
	title := strings.TrimSpace(it.Title)
	if title == "" {
		title = "(без заголовка)" // паттерн RenderMessageView
	}
	title = navTruncateTitle(title, photoCaptionTitleRunes)
	summary := truncateRunes(strings.TrimSpace(it.Summary), photoCaptionSummaryRunes)
	srcLine := ""
	if link := l3Link(it); link != "" {
		srcLine = "\n↪ <a href=\"" + link + "\">источник</a>"
	}
	cap := buildPhotoCaption(title, summary, srcLine, true)
	if len(cap) > telegram.PhotoCaptionMaxBytes {
		// (3): строка источника не влезает даже после ужатия title/summary —
		// отбрасываем её целиком и пережимаем компоненты в полный бюджет.
		cap = buildPhotoCaption(title, summary, "", false)
	}
	return cap
}

// buildPhotoCaption собирает caption из plain-text компонентов: title/summary
// ужимаются fitEscapedBytes в оставшийся бюджет (БАЙТЫ эскейпнутого
// фрагмента), srcLine — готовый фрагмент (l3Link уже экранирован/валидирован,
// резать его нельзя — битый URL). withSrc=false → srcLine игнорируется.
// Результат ≤PhotoCaptionMaxBytes и well-formed: теги добавляются ПОСЛЕ
// ужатия, сущности не режутся (резка по plain text до EscapeHTML).
func buildPhotoCaption(title, summary, srcLine string, withSrc bool) string {
	const fixed = len("<b>") + len("</b>")
	src := 0
	if withSrc {
		src = len(srcLine)
	}
	escTitle := telegram.EscapeHTML(title)
	escSummary := ""
	if summary != "" {
		// (1) summary: бюджет — что осталось после title и srcLine.
		budget := telegram.PhotoCaptionMaxBytes - fixed - len(escTitle) - len("\n") - src
		if budget >= len("…") {
			escSummary = telegram.EscapeHTML(fitEscapedBytes(summary, budget))
		}
	}
	sumPart := 0
	if escSummary != "" {
		sumPart = len("\n") + len(escSummary)
	}
	// (2) title: бюджет — что осталось после summary и srcLine.
	if budget := telegram.PhotoCaptionMaxBytes - fixed - sumPart - src; len(escTitle) > budget {
		escTitle = telegram.EscapeHTML(fitEscapedBytes(title, budget))
	}
	var b strings.Builder
	b.WriteString("<b>")
	b.WriteString(escTitle)
	b.WriteString("</b>")
	if escSummary != "" {
		b.WriteString("\n")
		b.WriteString(escSummary)
	}
	if withSrc {
		b.WriteString(srcLine)
	}
	return b.String()
}

// fitEscapedBytes ужимает s (plain text) до len(EscapeHTML(результата)) ≤
// budget: бинарный поиск по рунам (длина эскейпнутого префикса монотонна —
// детерминированно terminates), при обрезке добавляется «…» (EscapeHTML её
// не экранирует, входит в бюджет). Даже одна руна с «…» не влезает → ""
// (компонент отброшен). Резка ДО эскейпа — сущность «&amp;» не рвётся.
func fitEscapedBytes(s string, budget int) string {
	if budget < len("…") {
		return ""
	}
	if len(telegram.EscapeHTML(s)) <= budget {
		return s // в лимит уже укладывается — резать нечего
	}
	runes := []rune(s)
	lo, hi, best := 0, len(runes), -1
	for lo <= hi {
		mid := (lo + hi) / 2
		if len(telegram.EscapeHTML(string(runes[:mid])))+len("…") <= budget {
			best = mid
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	if best < 0 {
		return ""
	}
	return string(runes[:best]) + "…"
}
