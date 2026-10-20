// Drill-down L2/L3 (Stage 8 D-2, план §D): парсинг сырого callback_data
// («L2:<digestID>», «L3:<digestID>:<sourceID>»), рендеры и seam
// telegram.WithDrillDown — NewDrillDown(store, sources-снапшот). Данные
// строго из SQLite (rest-семантика §4.3): L2/L3-сообщения не хранятся и
// рендерятся по запросу. Весь динамический текст через telegram.EscapeHTML
// (§4.8); разметка создаётся только рендером; URL в <a href> — только
// http/https, проверяется перед вставкой (§4.8). Ошибка seam'а (мусорные
// data, неизвестный digest, сбой storage) → error: диспетчер D-1 сам
// ack'ит «Не получилось открыть» и логирует, паник здесь не должно быть.

package digest

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/akomyagin/aiTelegaBot/internal/storage"
	"github.com/akomyagin/aiTelegaBot/internal/telegram"
)

const (
	// l2MaxSections — план §D: секций >15 → без кнопок секций, только
	// «◀ Назад» + «Общий список» (L3:<did>:0) и пометка в тексте.
	l2MaxSections = 15
	// l3SnippetRunes — план §D: snippet в строке L3 ≤120 рун.
	l3SnippetRunes = 120
	// l2BtnSuffix/l2BtnBudget — лейбл кнопки секции «<источник> → сообщения».
	// УТОЧНЕНИЕ ПЛАНА: план писал «≤60 рун» ДО урока A2 H1 — лимит Bot API
	// 64 БАЙТА, поэтому бюджет динамической части байтовый:
	// 64 − len(" → сообщения") − len("…") = 64 − 23 − 3 = 38 (worst-case
	// лейбл 38+3+23 = ровно 64; TruncateButtonLabelTo режет на границе рун
	// и дописывает "…" только при обрезке). Не «правь» 38 вверх — впритык.
	l2BtnSuffix = " → сообщения"
	l2BtnBudget = 64 - len(l2BtnSuffix) - len("…")
	// drillBackLabel — кнопка «назад»: из L2 в L1, из L3 в L2 (§4.4).
	drillBackLabel = "◀ Назад"
	// l2TooManyNote — пометка при >l2MaxSections секциях (план §D).
	l2TooManyNote = "Слишком много источников — смотрите общий список."
	// l3AllHeader — заголовок общего списка L3:<did>:0.
	l3AllHeader = "Все сообщения"
)

// parseDrillDownData разбирает СЫРОЙ callback_data с префиксом (контракт
// D-1): «L2:<digestID>» → level 2, «L3:<digestID>:<sourceID>» → level 3
// (sourceID 0 = все элементы). Парсер — strings.CutPrefix + strings.Cut,
// не regex (§4.4). Мусор, не-числа, неположительный digestID, отрицательный
// sourceID и переполнение int64 → error, не паника.
func parseDrillDownData(data string) (level int, digestID, sourceID int64, err error) {
	if rest, ok := strings.CutPrefix(data, "L2:"); ok {
		id, idErr := parseIDBounded(rest, 1)
		if idErr != nil {
			return 0, 0, 0, fmt.Errorf("drill-down: bad L2 data %q: %w", data, idErr)
		}
		return 2, id, 0, nil
	}
	if rest, ok := strings.CutPrefix(data, "L3:"); ok {
		didStr, sidStr, found := strings.Cut(rest, ":")
		if !found {
			return 0, 0, 0, fmt.Errorf("drill-down: bad L3 data %q: want L3:<digestID>:<sourceID>", data)
		}
		did, idErr := parseIDBounded(didStr, 1)
		if idErr != nil {
			return 0, 0, 0, fmt.Errorf("drill-down: bad L3 digest id %q: %w", data, idErr)
		}
		sid, idErr := parseIDBounded(sidStr, 0)
		if idErr != nil {
			return 0, 0, 0, fmt.Errorf("drill-down: bad L3 source id %q: %w", data, idErr)
		}
		return 3, did, sid, nil
	}
	return 0, 0, 0, fmt.Errorf("drill-down: unknown callback data %q", data)
}

// parseIDBounded парсит десятичный id и требует value >= min (digestID ≥ 1,
// sourceID ≥ 0). ParseInt сам отвергает мусор, «», переполнение int64 и
// значения ниже min.
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

// l2Section — одна группа элементов L2 по источнику (порядок первого
// появления, никаких map-итераций в рендер — golden детерминирован).
type l2Section struct {
	SourceID int64
	Name     string
	Items    []storage.DigestItem
}

// NewDrillDown собирает seam для telegram.WithDrillDown (план §D):
// telegram.DrillDown = func(ctx, data string) (*CallbackResult, error),
// data приходит СЫРЫМ с префиксом — парсинг здесь. sources — снапшот строк
// sources для имён источников (в app: store.ListSources, один вызов на
// drill-down, кэш не нужен — single-user, таблица маленькая); storage.Source
// не имеет поля Name — отображаемое имя = Ref (RSS URL / username канала).
// source_id без строки → «источник #N» (план §D).
func NewDrillDown(store storage.Store, sources func(ctx context.Context) ([]storage.Source, error)) telegram.DrillDown {
	return func(ctx context.Context, data string) (*telegram.CallbackResult, error) {
		level, digestID, sourceID, err := parseDrillDownData(data)
		if err != nil {
			return nil, err
		}
		d, ok, err := store.GetDigest(ctx, digestID)
		if err != nil {
			return nil, fmt.Errorf("drill-down: GetDigest(%d): %w", digestID, err)
		}
		if !ok {
			// Crafted/устаревший id — error (ack+лог диспетчера), НЕ паника.
			return nil, fmt.Errorf("drill-down: digest %d не существует", digestID)
		}
		names, err := sourceNames(ctx, sources)
		if err != nil {
			return nil, err
		}

		if level == 2 {
			topicName, err := drillTopicName(ctx, store, d.TopicID)
			if err != nil {
				return nil, err
			}
			items, err := store.ListDigestItems(ctx, digestID)
			if err != nil {
				return nil, fmt.Errorf("drill-down: ListDigestItems(%d): %w", digestID, err)
			}
			return renderDrillL2(topicName, groupBySource(items, names), digestID), nil
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
		header := l3AllHeader
		if sourceID != 0 {
			name, ok := names[sourceID]
			if !ok || strings.TrimSpace(name) == "" {
				name = "источник #" + strconv.FormatInt(sourceID, 10)
			}
			header = name
		}
		return renderDrillL3(header, items, digestID), nil
	}
}

// sourceNames — снапшот id → отображаемое имя источника.
func sourceNames(ctx context.Context, sources func(ctx context.Context) ([]storage.Source, error)) (map[int64]string, error) {
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
		names[s.ID] = s.Ref
	}
	return names, nil
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

// groupBySource группирует элементы по SourceID с СОХРАНЕНИЕМ порядка
// первого появления (план §D: детерминированный golden, без map-итераций).
func groupBySource(items []storage.DigestItem, names map[int64]string) []l2Section {
	idx := make(map[int64]int, len(items))
	var sections []l2Section
	for _, it := range items {
		if i, ok := idx[it.SourceID]; ok {
			sections[i].Items = append(sections[i].Items, it)
			continue
		}
		name, ok := names[it.SourceID]
		if !ok || strings.TrimSpace(name) == "" {
			name = "источник #" + strconv.FormatInt(it.SourceID, 10)
		}
		idx[it.SourceID] = len(sections)
		sections = append(sections, l2Section{
			SourceID: it.SourceID,
			Name:     name,
			Items:    []storage.DigestItem{it},
		})
	}
	return sections
}

// renderDrillL2 — рендер «L2:<digestID>» (план §D):
//
//	📰 <тема> — по источникам
//
//	<b><имя источника></b>
//	• <summary>            ← пустой summary → title
//	…
//
// Кнопки: по одной «<имя> → сообщения» (L3:<did>:<srcID>) на секцию +
// «◀ Назад» (L1:<did>). Секций >15 — только «Общий список» (L3:<did>:0)
// и «◀ Назад» + пометка в тексте.
func renderDrillL2(topicName string, sections []l2Section, digestID int64) *telegram.CallbackResult {
	did := strconv.FormatInt(digestID, 10)
	header := "📰 " + telegram.EscapeHTML(topicName) + " — по источникам"
	back := []telegram.InlineButton{{Text: drillBackLabel, Data: "L1:" + did}}

	if len(sections) > l2MaxSections {
		return &telegram.CallbackResult{Edit: &telegram.EditMessage{
			// Ревью D L3: ветка тоже проходит guard 4096 (гигантское имя
			// темы + пометка теоретически могут выйти за лимит).
			Text: fitDrillDownList(header, []string{l2TooManyNote}, "\n\n"),
			Buttons: [][]telegram.InlineButton{
				{{Text: "Общий список", Data: "L3:" + did + ":0"}},
				back,
			},
		}}
	}

	blocks := make([]string, 0, len(sections))
	buttons := make([][]telegram.InlineButton, 0, len(sections)+1)
	for _, s := range sections {
		var b strings.Builder
		b.WriteString("<b>")
		b.WriteString(telegram.EscapeHTML(s.Name))
		b.WriteString("</b>")
		for _, it := range s.Items {
			line := strings.TrimSpace(it.Summary)
			if line == "" {
				line = it.Title // пустой summary → title (план §D)
			}
			b.WriteString("\n• ")
			b.WriteString(telegram.EscapeHTML(line))
		}
		blocks = append(blocks, b.String())
		// Ревью D M1: SourceID==0 — элементы с неизвестной принадлежностью
		// (NULL source_id → COALESCE 0), а «L3:<did>:0» — сентинел «ВСЕ
		// элементы». Кнопка секции обещала бы фильтр, открывая общий список,
		// поэтому для 0-секции кнопки нет (текст секции остаётся).
		if s.SourceID != 0 {
			buttons = append(buttons, []telegram.InlineButton{{
				Text: telegram.TruncateButtonLabelTo(s.Name, l2BtnBudget) + l2BtnSuffix,
				Data: "L3:" + did + ":" + strconv.FormatInt(s.SourceID, 10),
			}})
		}
	}
	buttons = append(buttons, back)

	return &telegram.CallbackResult{Edit: &telegram.EditMessage{
		Text:    fitDrillDownList(header, blocks, "\n\n"),
		Buttons: buttons,
	}}
}

// renderDrillL3 — рендер «L3:<digestID>:<sourceID|0>» (план §D):
//
//	📄 <имя источника|«Все сообщения»>
//
//	• <a href="LINK">title</a> — <snippet ≤120 рун>
//
// LINK: url со схемой http/https (§4.8) → url; иначе deep link из
// tg_peer/tg_msg_id; и deep link пуст → БЕЗ <a>. Пустой snippet → строка
// без « — ». Кнопка «◀ Назад» → L2:<digestID>. Guard >4096 рун —
// fitDrillDownList («…и ещё K»).
func renderDrillL3(headerName string, items []storage.DigestItem, digestID int64) *telegram.CallbackResult {
	header := "📄 " + telegram.EscapeHTML(headerName)
	lines := make([]string, 0, len(items))
	for _, it := range items {
		var b strings.Builder
		b.WriteString("• ")
		// Ревью D L7: пустой title при живой ссылке дал бы пустой <a></a> —
		// fallback на placeholder, строка остаётся кликабельной.
		raw := strings.TrimSpace(it.Title)
		if raw == "" {
			raw = "(без заголовка)"
		}
		title := telegram.EscapeHTML(raw)
		if link := l3Link(it); link != "" {
			b.WriteString(`<a href="`)
			b.WriteString(link)
			b.WriteString(`">`)
			b.WriteString(title)
			b.WriteString("</a>")
		} else {
			b.WriteString(title)
		}
		if snip := strings.TrimSpace(it.Snippet); snip != "" {
			b.WriteString(" — ")
			b.WriteString(telegram.EscapeHTML(truncateRunes(snip, l3SnippetRunes)))
		}
		lines = append(lines, b.String())
	}
	return &telegram.CallbackResult{Edit: &telegram.EditMessage{
		Text: fitDrillDownList(header, lines, "\n"),
		Buttons: [][]telegram.InlineButton{
			{{Text: drillBackLabel, Data: "L2:" + strconv.FormatInt(digestID, 10)}},
		},
	}}
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

// fitDrillDownList — локальный guard 4096 рун для L2/L3 (паттерн
// RenderTopicL1, план §D «обрезка с хвоста с пометкой “…и ещё K”»;
// сигнатура RenderTopicL1 заморожена для D, поэтому реализация локальная).
// Хвостовые блоки отбрасываются по одному, K = число отброшенных БЛОКОВ
// (в L2 блок = секция). Полоса M1 (ревью C1): цикл не тестирует «заголовок
// allein» — если заголовок ≤4096 рун, он возвращается как есть; длиннее —
// режется по рунам с «…».
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
