// Новые рендеры четырёхуровневой навигации (Этап 9D-1, план §4.6 и «Подэтап
// 9D»): RenderTopicView (L1 тема), RenderSourceView (L2 топик),
// RenderMessageView (L3 сообщение). Блок общего саммари TOC — в render.go
// (RenderTOC +overall).
//
// Рендеры НЕ подключены к Run/drilldown в 9D-1 — ноль изменения поведения
// бота; 9D-2 подключает их (диспетчер callback'ов S:/M:/P:/PDEL + legacy
// alias'ы, pipeline шаги 4d/6/7, app-wiring) и удаляет легаси
// RenderTopicBody/RenderTopicL1/renderDrillL2/renderDrillL3 вместе с их
// golden. ОТКЛОНЕНИЕ 9D-1 (отчёт): новый код в отдельном файле, а не в
// render.go — аддитивную часть проще ревьюить; файловая таблица плана не
// контракт хранения.
//
// Весь динамический текст через telegram.EscapeHTML (§4.8); URL в <a href> —
// только http/https, кавычка отвергается ПЕРЕД вставкой (l3Link-паттерн);
// разметка создаётся только рендером. Детерминизм golden: порядок срезов,
// никаких map-итераций.

package digest

import (
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/akomyagin/aiTelegaBot/internal/llm"
	"github.com/akomyagin/aiTelegaBot/internal/storage"
	"github.com/akomyagin/aiTelegaBot/internal/telegram"
)

const (
	// tocOverallRunes — защитный предел overall-блока TOC (вызов D обещает
	// 3–5 предложений; стиль ревью C1 M2: динамические части режутся в
	// рендере, а не полагаются на вызывающего).
	tocOverallRunes = 1000
	// topicNameBtnBudget — бюджет лейбла кнопки-имени топика: полные 64
	// БАЙТА Bot API минус «…» (стиль Этапа 8 B: у лейбла нет префикса;
	// TruncateButtonLabelTo режет на границе рун и дописывает «…» только при
	// обрезке — worst-case лейбл ровно 64 байта). Полное имя остаётся в
	// текстовой строке L1 (план §4.6).
	topicNameBtnBudget = 64 - len("…")
	// topicViewSummaryRunes — предел саммари топика в L1-строке и в шапке/
	// подзаголовке L2 (вызов C обещает 1–2 предложения; защитная резка).
	topicViewSummaryRunes = 300
	// sourceSummaryRunes — предел саммари сообщения в L2-блоке: 9B уже
	// держит summary ≤600 рун (включая «↳»-хвосты статей), резка здесь —
	// защита от legacy/патологических строк.
	sourceSummaryRunes = 600
	// sourceViewMaxItems — план 9D: >20 сообщений в L2 → первые 20 + «…и ещё
	// K» (пагинации нет — триггер §7; кнопки строк и так съедят место).
	sourceViewMaxItems = 20
	// messageLinkTitleRunes/messageLinkSummaryRunes — пределы строки блока
	// «Ссылки:» в L3.
	messageLinkTitleRunes   = 120
	messageLinkSummaryRunes = 300
	// navHeaderTitleRunes — предел title в шапке L3 (ревью 9D M1:
	// digest_items.title пишется БЕЗ резки — для tg_*-источников это первая
	// строка сообщения до ~4096 рун, а EscapeHTML ещё удлиняет; без лимита
	// текст L3 превышает 4096 → MESSAGE_TOO_LONG, view не открывается.
	// Полный текст и так ниже в full_text).
	navHeaderTitleRunes = 300
	// navBlockTitleRunes — предел title внутри блока L2 (ревью 9D L1:
	// сверхдлинный блок иначе молча отбрасывается guard'ом fitDrillDownList;
	// паттерн topicTitleRunes Этапа 8).
	navBlockTitleRunes = 120
	// Лейблы кнопок новой навигации (план §4.6).
	sourceReadLabel   = "Читать ▸"
	messagePhotoLabel = "🖼 Фото"
	messageOpenLabel  = "↗ Перейти в источник"
	backToTOCLabel    = "◀ К оглавлению"
	// messageViewCutMarker — пометка резки full_text в L3 (план §4.6
	// M:-view: «…полный текст — в источнике»).
	messageViewCutMarker = "…полный текст — в источнике"
	// sourceViewEmptyNote — крайний случай §4.6: S: у digest без items
	// (crash delivered=0, кейс Этапа 8).
	sourceViewEmptyNote = "Материалов нет"
	// messageViewLegacyNote — пометка L3 для legacy-строк без full_text
	// (план §4.6: snippet + «(сохранён только фрагмент — legacy)»).
	messageViewLegacyNote = "(сохранён только фрагмент — legacy)"
	// messageViewNoTextNote — защитная строка: и full_text, и snippet пусты.
	messageViewNoTextNote = "(текст не сохранён)"
	// messageMediaMark — маркер медиа в L2-блоке (заготовка 9E: рендерится
	// уже здесь при media_ref≠'', план 9D тест-кейс 4).
	messageMediaMark = "🖼"
)

// URLButton — внешняя URL-кнопка (план §4.6: «Перейти»/«перейти в источник» —
// единственная точка выхода из бота). telegram.InlineButton поля URL ещё НЕ
// имеет — его добавит 9D-2 (план 9D: internal/telegram/bot.go), поэтому
// URL-кнопки возвращаются отдельным типизированным списком NavView.URLButtons,
// а в NavView.Buttons стоят nil-строки-плейсхолдеры (см. NavView).
type URLButton struct{ Text, URL string }

// NavView — результат новых рендеров 9D (КОНТРАКТ ДЛЯ 9D-2, решение 9D-1):
//
//   - Text — готовый HTML под ParseMode HTML: всё динамическое
//     экранировано рендером, telegram НЕ переэкранирует (паттерн
//     DigestView/EditMessage);
//   - Buttons — строки клавиатуры в визуальном порядке: либо строка
//     telegram.InlineButton (Text + Data: S:/M:/P:/T:/TOC:), либо NIL-строка
//     — плейсхолдер очередной URL-кнопки;
//   - URLButtons — внешние URL-кнопки (http/https или t.me deep link,
//     проверены рендером).
//
// Правило сборки 9D-2: обход Buttons по порядку; i-я nil-строка заменяется
// строкой {InlineButton{Text: URLButtons[i].Text, URL: URLButtons[i].URL}}
// (после добавления InlineButton.URL; инвариант: число nil-строк равно
// len(URLButtons)). Схема выбрана, чтобы сохранить визуальный порядок плана
// §4.6 для L3 (🖼 Фото → ↗ Перейти в источник → ◀ Назад), не меняя
// telegram.InlineButton в 9D-1.
type NavView struct {
	Text       string
	Buttons    [][]telegram.InlineButton
	URLButtons []URLButton
}

// TopicEntry — один топик (источник) темы в L1-view (план §4.6/9D): имя
// топика = отображаемое имя источника (снапшот sources, для tg_* — fallback
// peer-Title; собирает 9D-2), ItemCount — число сообщений, Summary — саммари
// топика из вызова C (пустое → строка без саммари, деградация плана).
// SourceID=0 — неизвестный источник: строка рендерится, кнопки НЕТ
// (S:<did>:0 — сентинел «все топики», паттерн ревью D M1).
type TopicEntry struct {
	SourceID  int64
	Name      string
	ItemCount int
	Summary   string
}

// RenderTopicView — L1-view темы (план §4.6/9D, заменяет RenderTopicL1 в
// 9D-2): шапка темы (сохранённый digests.body КАК ЕСТЬ — уже HTML, никогда не
// переэкранировать) + «Топики:» + по топику строка
//
//   - <b>имя</b> — N сообщ.: саммари топика
//
// + кнопки-имена S:<did>:<sid> по одной в ряд (лейбл
// TruncateButtonLabelTo ≤64 БАЙТ — жёсткий лимит Bot API, полное имя остаётся
// в тексте) + «◀ К оглавлению» TOC:<runID> (runID=0 → legacy-прогон, кнопки
// нет). Пустой список топиков → только body (деградация). Guard 4096 —
// fitDrillDownList БЛОКАМИ-топиками («…и ещё K»); кнопки отброшенных guard'ом
// строк остаются (имя на кнопке самодостаточно).
func RenderTopicView(body string, digestID, runID int64, topics []TopicEntry) *NavView {
	header := body
	blocks := make([]string, 0, len(topics))
	if len(topics) > 0 {
		header = body + "\n\nТопики:"
		for _, tp := range topics {
			var b strings.Builder
			b.WriteString("• <b>")
			b.WriteString(telegram.EscapeHTML(topicEntryName(tp.Name, tp.SourceID)))
			b.WriteString("</b> — ")
			b.WriteString(strconv.Itoa(tp.ItemCount))
			b.WriteString(" сообщ.")
			if s := strings.TrimSpace(tp.Summary); s != "" {
				b.WriteString(": ")
				b.WriteString(telegram.EscapeHTML(truncateRunes(s, topicViewSummaryRunes)))
			}
			blocks = append(blocks, b.String())
		}
	}
	view := &NavView{Text: fitDrillDownList(header, blocks, "\n")}
	for _, tp := range topics {
		if tp.SourceID == 0 {
			continue // неизвестный источник — строка есть, кнопки нет (см. TopicEntry)
		}
		view.Buttons = append(view.Buttons, []telegram.InlineButton{{
			Text: telegram.TruncateButtonLabelTo(topicEntryName(tp.Name, tp.SourceID), topicNameBtnBudget),
			Data: "S:" + strconv.FormatInt(digestID, 10) + ":" + strconv.FormatInt(tp.SourceID, 10),
		}})
	}
	if runID > 0 {
		view.Buttons = append(view.Buttons, []telegram.InlineButton{{
			Text: backToTOCLabel,
			Data: "TOC:" + strconv.FormatInt(runID, 10),
		}})
	}
	return view
}

// topicEntryName — отображаемое имя топика с fallback «источник #N»
// (groupBySource-паттерн: пустое имя не рендерится пустым <b></b>).
func topicEntryName(name string, sourceID int64) string {
	if n := strings.TrimSpace(name); n != "" {
		return n
	}
	return "источник #" + strconv.FormatInt(sourceID, 10)
}

// navTruncateTitle — резка title до n рун с явной пометкой «…» при обрезке
// (ревью 9D M1/L1). Вызывать ДО EscapeHTML — экранирование (&→&amp;) само
// удлиняет строку, поэтому резать нужно исходные руны.
func navTruncateTitle(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return truncateRunes(s, n) + "…"
}

// SourceSection — один топик (источник) L2-view: Name/Summary — шапка и
// саммари топика (вызов C), Items — сообщения в порядке ListDigestItems
// (id ASC). Порядок среза задаёт вызывающий (9D-2 — groupBySource):
// детерминированный golden, никаких map-итераций.
type SourceSection struct {
	SourceID int64
	Name     string
	Summary  string
	Items    []storage.DigestItem
}

// RenderSourceView — L2-view топика (план §4.6/9D, заменяет renderDrillL2/
// renderDrillL3 в 9D-2). sourceID != 0 → ОДНА секция: шапка
// «📰 <тема> / <имя топика>» + саммари топика; sourceID == 0 → все топики с
// подзаголовками «<b>имя</b>» (+ саммари топика). По сообщению блок:
//
//   - [🖼] <a href="LINK">title</a>   ← 🖼 при media_ref≠” (заготовка 9E); LINK — l3Link-паттерн
//     <summary>                        ← пустой summary → title; «↳»-строки с отступом
//     ↪ <a href="LINK">перейти</a>     ← только при живом LINK
//
// Кнопки: «Читать ▸» M:<itemID> на строку (course-строка — БЕЗ кнопки, блок
// «• 🎓 <фраза>» + «перейти») + «◀ Назад» T:<did>. >sourceViewMaxItems
// сообщений → первые 20 + «…и ещё K» (маркер приклеен к последнему показанному
// блоку — guard 4096 не вылезет); guard 4096 — fitDrillDownList
// блоками-сообщениями. Digest без items → «Материалов нет» + «◀ Назад»
// (крайний случай §4.6).
func RenderSourceView(topicName string, digestID, sourceID int64, sections []SourceSection) *NavView {
	did := strconv.FormatInt(digestID, 10)
	back := []telegram.InlineButton{{Text: drillBackLabel, Data: "T:" + did}}

	header := "📰 " + telegram.EscapeHTML(topicName)
	if sourceID != 0 && len(sections) == 1 {
		header += " / " + telegram.EscapeHTML(topicEntryName(sections[0].Name, sourceID))
		if s := strings.TrimSpace(sections[0].Summary); s != "" {
			header += "\n\n" + telegram.EscapeHTML(truncateRunes(s, topicViewSummaryRunes))
		}
	}

	total := 0
	for _, sec := range sections {
		total += len(sec.Items)
	}
	if total == 0 {
		return &NavView{
			Text:    header + "\n\n" + sourceViewEmptyNote,
			Buttons: [][]telegram.InlineButton{back},
		}
	}

	var blocks []string
	var buttons [][]telegram.InlineButton
	shown := 0
	truncated := false
loop:
	for _, sec := range sections {
		// Ревью 9D L6: подзаголовок добавляется ЛЕНИВО — только перед первым
		// ПОКАЗАННЫМ элементом секции. Иначе при truncation ровно на границе
		// секций (shown≥20) рендерился подзаголовок следующей секции с нулём
		// элементов и приклеенным «…и ещё K».
		sub := ""
		if sourceID == 0 {
			sub = "<b>" + telegram.EscapeHTML(topicEntryName(sec.Name, sec.SourceID)) + "</b>"
			if s := strings.TrimSpace(sec.Summary); s != "" {
				sub += "\n" + telegram.EscapeHTML(truncateRunes(s, topicViewSummaryRunes))
			}
		}
		for _, it := range sec.Items {
			if shown >= sourceViewMaxItems {
				truncated = true
				break loop
			}
			if sub != "" {
				blocks = append(blocks, sub)
				sub = ""
			}
			block, btn := renderSourceItemBlock(it)
			blocks = append(blocks, block)
			if btn != nil {
				buttons = append(buttons, btn)
			}
			shown++
		}
	}
	if truncated {
		// K = число НЕПОКАЗАННЫХ сообщений (считаем блоками-сообщениями,
		// подзаголовки секций не в счёт).
		blocks[len(blocks)-1] += "\n…и ещё " + strconv.Itoa(total-shown)
	}
	buttons = append(buttons, back)
	return &NavView{Text: fitDrillDownList(header, blocks, "\n\n"), Buttons: buttons}
}

// renderSourceItemBlock — один блок сообщения L2 (см. RenderSourceView).
// Второе возвращаемое значение — строка кнопки «Читать ▸» M:<itemID> либо nil
// (course — план 9D: курс-строка без кнопки чтения; кнопка всё равно ведёт
// в M:-view, но план предписывает её не давать).
func renderSourceItemBlock(it storage.DigestItem) (string, []telegram.InlineButton) {
	link := l3Link(it)
	line := strings.TrimSpace(it.Summary)
	if line == "" {
		line = it.Title // пустой summary → title (план 9D)
	}

	var b strings.Builder
	if it.Class == llm.ClassCourse {
		// Курс-строка: саммари вызова A уже имеет форму «Курс: <название> —
		// <тема>» (9A), поэтому префикс — только «🎓 », без второго «Курс:».
		b.WriteString("• 🎓 ")
		b.WriteString(telegram.EscapeHTML(truncateRunes(line, sourceSummaryRunes)))
		if link != "" {
			b.WriteString("\n  ↪ <a href=\"")
			b.WriteString(link)
			b.WriteString("\">перейти</a>")
		}
		return b.String(), nil
	}

	b.WriteString("• ")
	if it.MediaRef != "" {
		b.WriteString(messageMediaMark + " ")
	}
	title := strings.TrimSpace(it.Title)
	if title == "" {
		title = "(без заголовка)" // паттерн ревью D L7: пустой <a></a> не рендерим
	}
	escTitle := telegram.EscapeHTML(navTruncateTitle(title, navBlockTitleRunes)) // ревью 9D L1
	if link != "" {
		b.WriteString(`<a href="`)
		b.WriteString(link)
		b.WriteString(`">`)
		b.WriteString(escTitle)
		b.WriteString("</a>")
	} else {
		b.WriteString(escTitle)
	}
	sum := telegram.EscapeHTML(truncateRunes(line, sourceSummaryRunes))
	// Многострочные саммари с «↳»-хвостами 9B — с отступом в два пробела
	// (техдолг F10 9B закрыт здесь: продолжение строки выравнивается под
	// маркер «• »).
	sum = strings.ReplaceAll(sum, "\n", "\n  ")
	b.WriteString("\n  ")
	b.WriteString(sum)
	if link != "" {
		b.WriteString("\n  ↪ <a href=\"")
		b.WriteString(link)
		b.WriteString("\">перейти</a>")
	}
	btn := []telegram.InlineButton{{
		Text: sourceReadLabel,
		Data: "M:" + strconv.FormatInt(it.ID, 10),
	}}
	return b.String(), btn
}

// MessageLink — одна строка блока «Ссылки:» L3-view: связка item_links +
// link_cache (9D-2 собирает через ListItemLinks(dedup_key) + GetLinkCache).
// Kind — вид ссылки (§4.3), Title/Summary — из link_cache (оба могут быть
// пустыми — деградация 9B F2/F6).
type MessageLink struct {
	URL     string
	Kind    string
	Title   string
	Summary string
}

// RenderMessageView — L3-view сообщения (план §4.6 M:-view):
//
//	<b>title</b>
//
//	<full_text>            ← пуст → snippet + «(сохранён только фрагмент — legacy)»
//
//	Ссылки:                ← только при непустом блоке; href — http/https,
//	• <a href>📄 title/домен</a> — summary   кавычка отвергнута (l3Link-паттерн)
//
// Резка 4096: сначала жертвуется блок «Ссылки:» (ссылка доступна из L2
// «перейти»), затем full_text режется по рунам с пометкой «…полный текст — в
// источнике». Кнопки (порядок плана §4.6): «🖼 Фото» P:<itemID> — только при
// media_ref≠” (Этап 9E: seam возвращает CallbackResult{Send} — отдельное
// sendPhoto-сообщение c caption и кнопкой «✖ Закрыть» PDEL);
// «↗ Перейти в источник» — URLButton (nil-плейсхолдер в Buttons, см. NavView);
// «◀ Назад» S:<DigestID>:<SourceID> — did/sid из самой строки digest_items
// (DigestID заполняет GetDigestItem).
func RenderMessageView(it storage.DigestItem, links []MessageLink) *NavView {
	title := strings.TrimSpace(it.Title)
	if title == "" {
		title = "(без заголовка)"
	}
	// Ревью 9D M1: title режется ДО EscapeHTML — иначе header (и весь текст
	// L3) может превысить 4096 рун → MESSAGE_TOO_LONG.
	header := "<b>" + telegram.EscapeHTML(navTruncateTitle(title, navHeaderTitleRunes)) + "</b>"

	body := strings.TrimSpace(it.FullText)
	if body == "" {
		if body = strings.TrimSpace(it.Snippet); body != "" {
			body += "\n\n" + messageViewLegacyNote
		}
	}
	if body == "" {
		body = messageViewNoTextNote
	}

	linksBlock := renderMessageLinks(links)
	if linksBlock != "" {
		linksBlock = "Ссылки:\n" + linksBlock
	}

	full := header + "\n\n" + body
	tail := ""
	if linksBlock != "" {
		tail = "\n\n" + linksBlock
	}
	if utf8.RuneCountInString(full+tail) > maxBodyRunes {
		if utf8.RuneCountInString(full) <= maxBodyRunes {
			tail = "" // сначала жертвуем блоком ссылок
		} else {
			overhead := utf8.RuneCountInString(header) + 2 + utf8.RuneCountInString("\n"+messageViewCutMarker)
			avail := maxBodyRunes - overhead
			if avail < 0 {
				avail = 0
			}
			full = header + "\n\n" + truncateRunes(body, avail) + "\n" + messageViewCutMarker
			tail = ""
		}
	}

	view := &NavView{Text: full + tail}
	if it.MediaRef != "" {
		view.Buttons = append(view.Buttons, []telegram.InlineButton{{
			Text: messagePhotoLabel,
			Data: "P:" + strconv.FormatInt(it.ID, 10),
		}})
	}
	if u := rawItemLink(it); u != "" {
		view.Buttons = append(view.Buttons, nil) // плейсхолдер URLButtons[0] — см. NavView
		view.URLButtons = append(view.URLButtons, URLButton{Text: messageOpenLabel, URL: u})
	}
	view.Buttons = append(view.Buttons, []telegram.InlineButton{{
		Text: drillBackLabel,
		Data: "S:" + strconv.FormatInt(it.DigestID, 10) + ":" + strconv.FormatInt(it.SourceID, 10),
	}})
	return view
}

// renderMessageLinks — строки блока «Ссылки:» (план §4.6): каждая ссылка —
// «• <a href="URL">иконка title-или-домен</a> — summary». href — только
// http/https, кавычка в URL отвергается ПЕРЕД вставкой (l3Link-паттерн —
// кавычка сломала бы атрибут); невалидная ссылка пропускается ЦЕЛИКОМ
// (деградация, не ошибка). Title пуст → домен; домен пуст → сам URL.
func renderMessageLinks(links []MessageLink) string {
	lines := make([]string, 0, len(links))
	for _, l := range links {
		if l.URL == "" || strings.Contains(l.URL, `"`) {
			continue
		}
		u, err := url.Parse(l.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			continue
		}
		label := strings.TrimSpace(l.Title)
		if label == "" {
			label = u.Host
		}
		if label == "" {
			label = l.URL
		}
		var b strings.Builder
		b.WriteString("• <a href=\"")
		b.WriteString(telegram.EscapeHTML(l.URL))
		b.WriteString("\">")
		b.WriteString(messageLinkIcon(l.Kind))
		b.WriteString(" ")
		b.WriteString(telegram.EscapeHTML(truncateRunes(label, messageLinkTitleRunes)))
		b.WriteString("</a>")
		if s := strings.TrimSpace(l.Summary); s != "" {
			b.WriteString(" — ")
			b.WriteString(telegram.EscapeHTML(truncateRunes(s, messageLinkSummaryRunes)))
		}
		lines = append(lines, b.String())
	}
	return strings.Join(lines, "\n")
}

// messageLinkIcon — иконка вида ссылки (§4.3): 📄 article, 🎓 course,
// 📢 ad, 🔗 other/неизвестный (константы llm — единый источник строк видов).
func messageLinkIcon(kind string) string {
	switch kind {
	case llm.LinkKindArticle:
		return "📄"
	case llm.LinkKindCourse:
		return "🎓"
	case llm.LinkKindAd:
		return "📢"
	default:
		return "🔗"
	}
}

// rawItemLink — ссылка элемента для URL-кнопки (Bot API принимает СЫРОЙ URL —
// БЕЗ HTML-экранирования, в отличие от <a href>): http/https URL (кавычка
// отвергается — защита атрибута не нужна, но битый URL не проходит и здесь)
// или telegram.DeepLink; оба пусты/биты → "" (кнопки нет). Валидация
// изоморфна l3Link — ОСОЗНАННО не рефакторинг общей функции: l3Link заморожен
// golden'ами легаси-рендеров (нулевое изменение поведения — мандат 9D-1).
func rawItemLink(it storage.DigestItem) string {
	if it.URL != "" && !strings.Contains(it.URL, `"`) {
		if u, err := url.Parse(it.URL); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
			return it.URL
		}
	}
	return telegram.DeepLink(it.TgPeer, it.TgMsgID)
}
