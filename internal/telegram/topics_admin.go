package telegram

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/go-telegram/bot/models"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
)

// Stage 8 B: темы — команды /topics, /newtopic, /settopic и callback'и
// ts:/tl:/tn: suggested-потока (план §4.11). telegram НЕ импортирует storage
// (существующая граница): данные приходят через seam-замыкания, возвращающие
// view-структуры этого пакета, а рендер живёт здесь — чтобы его можно было
// golden-тестировать без БД. Все опции Bot для тем опциональны: при nil
// команда/кнопка деградирует в отказ «функция недоступна» (тот же паттерн,
// что dialogLister без MTProto).

const (
	// topicsPerPage — сколько тем на странице tl-пикера (план §4.11:
	// «кнопки ts:<N>:<id> по страницам до 8 штук»).
	topicsPerPage = 8
	// maxTopicNameRunes — потолок имени темы в подэтапе B (≤40 рун).
	// Валидация здесь, до вызова seam: storage.CreateTopic длину не
	// ограничивает, llm-слой режет по своим правилам.
	maxTopicNameRunes = 40

	// Callback-префиксы тем (план §4.4): парсер — strings.CutPrefix +
	// strings.Cut, не regex. Худший случай "ts:" + 19 + ":" + 19 = 41 байт;
	// "tl:" + 19 + ":" + 19 (offset) = 41 байт — оба < 64.
	cbPrefixTopicSet  = "ts:"
	cbPrefixTopicList = "tl:"
	cbPrefixTopicNone = "tn:"

	topicsUnavailableText      = "Темы появятся в следующих версиях"
	topicsErrorText            = "❌ Не удалось получить список тем."
	topicsEmptyText            = "Тем пока нет."
	topicCreateUsageText       = "Использование: /newtopic [имя темы] (до 40 символов)"
	topicNameTooLongText       = "❌ Имя темы длиннее 40 символов — придумайте короче."
	topicCreateUnavailableText = "Создание тем недоступно."
	topicCreateErrorText       = "❌ Не удалось создать тему."
	setTopicUsageText          = "Использование: /settopic [id источника] [id темы или имя] (см. /sources, /topics)"
	setTopicBadSourceIDText    = "❌ id источника должен быть числом (см. /sources)."
	setTopicErrorText          = "❌ Не удалось привязать тему."
	setTopicSourceNotFoundText = "Источника с id %d нет. /sources — список."
	setTopicNotFoundText       = "Темы «%s» нет. /topics — список."
	topicBindUnavailableText   = "Привязка тем недоступна."
	topicPickerUnavailableText = "Выбор темы недоступен."
	topicPickerErrorText       = "❌ Не удалось получить список тем."
	topicBadCallbackText       = "❌ Некорректный запрос."
	topicStaleText             = "❌ Тема не найдена — список устарел. Откройте /topics заново."
	staticSourceRemoveText     = "Статические источники управляются через .env."
	// tnAckText — ack на tn: (план §4.11: источник остаётся «Без темы»,
	// ничего не меняется; сообщение не редактируется, только ack).
	tnAckText = "Оставлено без темы"
)

// TopicSourceView — строка одного источника внутри темы (/topics).
type TopicSourceView struct {
	ID      int64
	Kind    string
	Ref     string
	Enabled bool
}

// TopicView — тема с её источниками. Count не хранится: выводится при
// рендере как len(Sources), чтобы две правды не разъезжались.
type TopicView struct {
	ID      int64
	Name    string
	Sources []TopicSourceView
}

// TopicLister возвращает темы с сгруппированными источниками для /topics и
// tl-пикера. На стороне seam (app): storage.ListTopics + storage.ListSources
// и группировка по COALESCE(topic_id,1). nil на Bot → «Темы появятся…».
type TopicLister func(ctx context.Context) ([]TopicView, error)

// NewTopicResult — исход /newtopic. storage.CreateTopic идемпотентен по
// UNIQUE name и возвращает только id, поэтому «создана»/«уже была»
// различает seam (например, сверяет имя с ListTopics до INSERT);
// telegram-слой лишь рендерит соответствующий ответ.
type NewTopicResult struct {
	ID      int64
	Created bool // false => тема с таким именем уже существовала
}

// TopicCreator создаёт тему (поверх storage.CreateTopic) с именем, уже
// проверенным telegram-слоем на длину (≤40 рун).
type TopicCreator func(ctx context.Context, name string) (NewTopicResult, error)

// TopicBindResult — исход привязки. storage.SetSourceTopic НЕ валидирует
// существование темы (FK не enforcement'ится — см. отчёт b-core), поэтому
// проверка источника и темы — задача seam (app): SourceFound/TopicFound.
type TopicBindResult struct {
	SourceFound bool
	TopicFound  bool
	TopicID     int64
	TopicName   string // имя разрешённой темы — для ответа (экранируется тут)
}

// TopicBinder привязывает источник к теме. topicArg: числовая строка → id
// темы; иначе → точное имя темы, разрешение имени в id происходит НА СТОРОНЕ
// seam (app) — telegram передаёт строку как есть. Используется и командой
// /settopic, и callback'ом ts: (там topicArg всегда числовой).
type TopicBinder func(ctx context.Context, sourceID int64, topicArg string) (TopicBindResult, error)

// TopicSuggestion — предложенная LLM тема, уже созданная в БД (§4.11 шаг 3:
// CreateTopic вызывается сразу, до показа кнопок).
type TopicSuggestion struct {
	TopicID   int64
	TopicName string
}

// TopicSuggester вызывается telegram-слоем после УСПЕШНОГО добавления
// источника (любой путь: /addsource, форвард, кнопка «Добавить» из /dialogs).
// sample — trial-элементы, собранные при валидации самого добавления
// (ForwardResult.Sample / SourceAddResult.Sample): ВТОРОЙ сетевой сбор не
// выполняется (ревью M3). Возвращает nil («нет предложения») при любом
// отказе — ошибка SuggestTopic, пустое имя: добавление тогда работает как
// раньше, без кнопок (обязательная деградация, план: «addSource без
// LLM-ключа не падает и не шлёт кнопки»; без ключа опция в app вообще не
// проводится — ревью M1). Всю работу делает seam (app): ListTopics,
// SuggestTopic (таймаут 15с), CreateTopic.
type TopicSuggester func(ctx context.Context, sourceID int64, kind, ref string, sample []feed.Item) *TopicSuggestion

// SourceRemoveResult — типизированный ответ расширенного remover'а
// (Stage 8 B): текущий контракт (found bool) не знает про origin, а
// /removesource для static-источника должен отказывать. Static=true →
// источник с origin='static': seam НЕ отключает его, telegram отвечает
// подсказкой про .env. Для dynamic поведение прежнее (Found).
type SourceRemoveResult struct {
	Found  bool
	Static bool
}

// SourceRemoverV2 — origin-aware версия WithSourceRemover (аддитивная:
// legacy-опция продолжает работать, V2 имеет приоритет).
type SourceRemoverV2 func(ctx context.Context, id int64) (SourceRemoveResult, error)

// handleTopics рендерит список тем с источниками: id, имя, count, по строке
// на источник (план, подэтап B).
func (b *Bot) handleTopics(ctx context.Context) {
	if b.topicLister == nil {
		b.reply(ctx, topicsUnavailableText)
		return
	}
	topics, err := b.topicLister(ctx)
	if err != nil {
		b.log.Error("listing topics failed", "error", err)
		b.reply(ctx, topicsErrorText)
		return
	}
	b.reply(ctx, renderTopicsList(topics))
}

// renderTopicsList — golden-тестируемый рендер /topics. Весь динамический
// текст (имена тем, kind, ref) экранируется: сообщение уходит с ParseMode
// HTML (план §4.8).
func renderTopicsList(topics []TopicView) string {
	if len(topics) == 0 {
		return topicsEmptyText
	}
	var sb strings.Builder
	sb.WriteString("Темы:\n")
	for _, t := range topics {
		fmt.Fprintf(&sb, "\n#%d %s — источников: %d\n", t.ID, EscapeHTML(t.Name), len(t.Sources))
		for _, s := range t.Sources {
			marker := ""
			if !s.Enabled {
				marker = " [отключён]"
			}
			fmt.Fprintf(&sb, "  • #%d %s: %s%s\n", s.ID, EscapeHTML(s.Kind), EscapeHTML(s.Ref), marker)
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

// handleNewTopic валидирует имя (непустое, ≤40 рун) ДО вызова seam и
// создаёт тему. Ответ различает «создана»/«уже была» по NewTopicResult.
func (b *Bot) handleNewTopic(ctx context.Context, arg string) {
	name := strings.TrimSpace(arg)
	if name == "" {
		b.reply(ctx, topicCreateUsageText)
		return
	}
	if utf8.RuneCountInString(name) > maxTopicNameRunes {
		b.reply(ctx, topicNameTooLongText)
		return
	}
	if b.topicCreator == nil {
		b.reply(ctx, topicCreateUnavailableText)
		return
	}
	res, err := b.topicCreator(ctx, name)
	if err != nil {
		b.log.Error("creating topic failed", "error", err)
		b.reply(ctx, topicCreateErrorText)
		return
	}
	if res.Created {
		b.reply(ctx, fmt.Sprintf("✅ Тема #%d создана: %s.", res.ID, EscapeHTML(name)))
		return
	}
	b.reply(ctx, fmt.Sprintf("Тема «%s» уже существует (#%d).", EscapeHTML(name), res.ID))
}

// handleSetTopic разбирает «/settopic <sourceID> <topicID|имя темы>»:
// первый токен — числовой id источника, остаток (может содержать пробелы —
// имя темы) передаётся seam как есть. Для static-источников привязка
// разрешена: никакого особого гейта здесь нет (план, строки 605–606:
// /settopic — главный способ дать тему arxiv/hn).
func (b *Bot) handleSetTopic(ctx context.Context, arg string) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		b.reply(ctx, setTopicUsageText)
		return
	}
	i := strings.IndexFunc(arg, unicode.IsSpace)
	if i < 0 {
		b.reply(ctx, setTopicUsageText) // нет аргумента темы
		return
	}
	sourceID, err := strconv.ParseInt(arg[:i], 10, 64)
	if err != nil {
		b.reply(ctx, setTopicBadSourceIDText)
		return
	}
	topicArg := strings.TrimSpace(arg[i+1:])
	if topicArg == "" {
		b.reply(ctx, setTopicUsageText)
		return
	}
	if b.topicBinder == nil {
		b.reply(ctx, topicBindUnavailableText)
		return
	}
	res, err := b.topicBinder(ctx, sourceID, topicArg)
	if err != nil {
		b.log.Error("binding topic failed", "error", err)
		b.reply(ctx, setTopicErrorText)
		return
	}
	b.replyBindResult(ctx, sourceID, topicArg, res)
}

// replyBindResult рендерит исход привязки — общий хелпер команды /settopic
// (текст ошибки — про имя/аргумент) ; callback ts: рендерит сам (там
// «не найдена тема» означает устаревшие данные кнопки, а не пользовательский
// аргумент).
func (b *Bot) replyBindResult(ctx context.Context, sourceID int64, topicArg string, res TopicBindResult) {
	switch {
	case !res.SourceFound:
		b.reply(ctx, fmt.Sprintf(setTopicSourceNotFoundText, sourceID))
	case !res.TopicFound:
		b.reply(ctx, fmt.Sprintf(setTopicNotFoundText, EscapeHTML(topicArg)))
	default:
		b.reply(ctx, bindSuccessText(sourceID, res.TopicName))
	}
}

func bindSuccessText(sourceID int64, topicName string) string {
	return fmt.Sprintf("✅ Источник #%d → тема %s.", sourceID, EscapeHTML(topicName))
}

// withTopicSuggestion — общий хелпер suggested-потока для ВСЕХ трёх путей
// добавления (план §4.11): доставляет базовый текст «источник добавлен»
// (регистраторы генерируют его как раньше) и, если TopicSuggester вернул
// предложение, добавляет строку «Предложенная тема: X» и ts/tl/tn-клавиатуру.
// Без опции / при nil-предложении — базовый текст без кнопок.
func (b *Bot) withTopicSuggestion(ctx context.Context, base string, sourceID int64, kind, ref string, sample []feed.Item) (string, *models.InlineKeyboardMarkup) {
	if b.topicSuggest == nil || sourceID == 0 {
		return base, nil
	}
	sug := b.topicSuggest(ctx, sourceID, kind, ref, sample)
	if sug == nil || sug.TopicID == 0 || strings.TrimSpace(sug.TopicName) == "" {
		return base, nil
	}
	text := base + "\n\nПредложенная тема: " + EscapeHTML(sug.TopicName)
	return text, suggestionKeyboard(sourceID, sug.TopicID)
}

// replyAdded доставляет ответ успешного добавления: текст + (опционально)
// клавиатура предложения. Используется /addsource и форвардом; диалоговая
// кнопка «Добавить» редактирует своё сообщение сама (editCallbackMessage).
func (b *Bot) replyAdded(ctx context.Context, base string, sourceID int64, kind, ref string, sample []feed.Item) {
	text, kb := b.withTopicSuggestion(ctx, base, sourceID, kind, ref, sample)
	if kb != nil {
		b.sendPage(ctx, text, kb)
		return
	}
	b.reply(ctx, text)
}

// suggestionKeyboard — кнопки подтверждения темы (§4.11 шаг 3):
// «Привязать» ts:<N>:<topicID>, «Другая тема» tl:<N>, «Без темы» tn:<N>.
// Лейблы статические — байтовый лимит 64 не нарушают.
func suggestionKeyboard(sourceID, topicID int64) *models.InlineKeyboardMarkup {
	sid := strconv.FormatInt(sourceID, 10)
	return &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{
		{
			{Text: "Привязать", CallbackData: cbPrefixTopicSet + sid + ":" + strconv.FormatInt(topicID, 10)},
			{Text: "Другая тема", CallbackData: cbPrefixTopicList + sid},
		},
		{{Text: "Без темы", CallbackData: cbPrefixTopicNone + sid}},
	}}
}

// handleTopicSetCallback обрабатывает "ts:<sourceID>:<topicID>": привязка
// через TopicBinder + edit «✅ Источник #N → тема X» (имя экранируется).
// Owner-чек и ack-на-каждый-callback — в диспетчере handleCallbackQuery
// (dialogs.go), inaccessible → только ack (pattern handleDialogAdd).
func (b *Bot) handleTopicSetCallback(ctx context.Context, q *models.CallbackQuery, arg string) {
	b.answerCallback(ctx, q.ID, "") // сначала ack, потом edit
	msg, ok := callbackMessage(q)
	if !ok {
		return // inaccessible message: ack only
	}

	sourceID, topicID, err := parseTopicSetArg(arg)
	if err != nil {
		b.log.Info("bad ts: callback data", "data_prefix", cbPrefixTopicSet, "error", err)
		b.editCallbackMessage(ctx, msg, topicBadCallbackText, nil)
		return
	}
	if b.topicBinder == nil {
		b.editCallbackMessage(ctx, msg, topicBindUnavailableText, nil)
		return
	}
	res, err := b.topicBinder(ctx, sourceID, strconv.FormatInt(topicID, 10))
	if err != nil {
		b.log.Error("binding topic failed", "error", err)
		b.editCallbackMessage(ctx, msg, setTopicErrorText, nil)
		return
	}
	switch {
	case !res.SourceFound:
		b.editCallbackMessage(ctx, msg, fmt.Sprintf(setTopicSourceNotFoundText, sourceID), nil)
	case !res.TopicFound:
		// Данные кнопки устарели (список тем перерисован/тема удалена).
		b.editCallbackMessage(ctx, msg, topicStaleText, nil)
	default:
		b.editCallbackMessage(ctx, msg, bindSuccessText(sourceID, res.TopicName), nil)
	}
}

// parseTopicSetArg разбирает "<sourceID>:<topicID>" из callback_data —
// strings.Cut + ParseInt, не regex (план §4.4).
func parseTopicSetArg(arg string) (sourceID, topicID int64, err error) {
	srcStr, topicStr, ok := strings.Cut(arg, ":")
	if !ok {
		return 0, 0, fmt.Errorf("telegram: ts: data without topic id")
	}
	if sourceID, err = strconv.ParseInt(srcStr, 10, 64); err != nil {
		return 0, 0, fmt.Errorf("telegram: ts: bad source id: %w", err)
	}
	if topicID, err = strconv.ParseInt(topicStr, 10, 64); err != nil {
		return 0, 0, fmt.Errorf("telegram: ts: bad topic id: %w", err)
	}
	return sourceID, topicID, nil
}

// handleTopicListCallback обрабатывает "tl:<sourceID>" и
// "tl:<sourceID>:<offset>": edit сообщения в список тем с кнопками
// ts:<N>:<id> по 8 на страницу (§4.11 шаг 4).
func (b *Bot) handleTopicListCallback(ctx context.Context, q *models.CallbackQuery, arg string) {
	b.answerCallback(ctx, q.ID, "")
	msg, ok := callbackMessage(q)
	if !ok {
		return
	}

	srcStr, offStr, hasOffset := strings.Cut(arg, ":")
	sourceID, err := strconv.ParseInt(srcStr, 10, 64)
	offset := 0
	if err == nil && hasOffset {
		offset, err = strconv.Atoi(offStr)
		if err == nil && offset < 0 {
			err = fmt.Errorf("telegram: tl: negative offset")
		}
	}
	if err != nil {
		b.log.Info("bad tl: callback data", "error", err)
		b.editCallbackMessage(ctx, msg, topicBadCallbackText, nil)
		return
	}
	if b.topicLister == nil {
		b.editCallbackMessage(ctx, msg, topicPickerUnavailableText, nil)
		return
	}
	topics, err := b.topicLister(ctx)
	if err != nil {
		b.log.Error("listing topics failed", "error", err)
		b.editCallbackMessage(ctx, msg, topicPickerErrorText, nil)
		return
	}
	text, kb := renderTopicPickerPage(topics, sourceID, offset)
	b.editCallbackMessage(ctx, msg, text, kb)
}

// handleTopicNoneCallback обрабатывает "tn:<sourceID>": ничего не меняет
// (источник остаётся «Без темы» — topic_id NULL ≡ тема 1), только ack.
// Сообщение не редактируется: план требует лишь ack «Оставлено без темы».
func (b *Bot) handleTopicNoneCallback(ctx context.Context, q *models.CallbackQuery, arg string) {
	_ = arg // sourceID не нужен: состояние не меняется
	b.answerCallback(ctx, q.ID, tnAckText)
}

// topicLabelBudget — байтовый бюджет имени темы в кнопке ts: Bot API
// отвергает лейблы длиннее 64 БАЙТ (BUTTON_TEXT_INVALID, урок A2 review H1).
// Имя темы — до 40 рун, т.е. до ~120 байт в кириллице, поэтому усечение
// обязательно. Бюджет: 64 − len("…").
const topicLabelBudget = 64 - len("…")

// renderTopicPickerPage рендерит страницу списка тем для привязки источника
// sourceID: строка-кнопка «имя темы» → ts:<sourceID>:<topicID> на каждую
// тему (≤ topicsPerPage) и навигация «◀ / ▶» → tl:<sourceID>:<offset>.
//
// ОТКЛОНЕНИЕ от буквы плана §4.11 («dp-пагинация переиспользуется») —
// осознанное: переиспользуется МЕХАНИКА (кламп и выравнивание offset — те же
// формулы, что в renderDialogsPage), но НЕ префикс dp:. dp: занят пейджингом
// /dialogs, и его обработчик перерисовывает ДИАЛОГИ — dp: на сообщении со
// списком тем перебросил бы пользователя в другой список. Пагинация тем
// живёт внутри собственного префикса: "tl:<sourceID>:<offset>", худший
// случай 3+19+1+19 = 42 байта < 64.
func renderTopicPickerPage(topics []TopicView, sourceID int64, offset int) (string, *models.InlineKeyboardMarkup) {
	if len(topics) == 0 {
		return topicsEmptyText, nil
	}
	if offset < 0 {
		offset = 0
	}
	if offset >= len(topics) {
		offset = (len(topics) - 1) / topicsPerPage * topicsPerPage
	}
	// Выравнивание «рукотворного» offset (tl:5:3) на начало страницы — та же
	// механика, что в renderDialogsPage (review L3).
	offset = offset / topicsPerPage * topicsPerPage
	page := topics[offset:]
	if len(page) > topicsPerPage {
		page = page[:topicsPerPage]
	}

	total := (len(topics) + topicsPerPage - 1) / topicsPerPage
	current := offset/topicsPerPage + 1

	text := fmt.Sprintf("Выберите тему для источника #%d (страница %d из %d):", sourceID, current, total)
	sid := strconv.FormatInt(sourceID, 10)
	rows := make([][]models.InlineKeyboardButton, 0, len(page)+1)
	for _, t := range page {
		label := t.Name
		if strings.TrimSpace(label) == "" {
			// Пустое имя недопустимо в лейбле кнопки — запасной вариант.
			label = "Тема #" + strconv.FormatInt(t.ID, 10)
		}
		rows = append(rows, []models.InlineKeyboardButton{{
			Text:         TruncateButtonLabelTo(label, topicLabelBudget),
			CallbackData: cbPrefixTopicSet + sid + ":" + strconv.FormatInt(t.ID, 10),
		}})
	}

	var nav []models.InlineKeyboardButton
	if offset > 0 {
		nav = append(nav, models.InlineKeyboardButton{
			Text:         "◀",
			CallbackData: cbPrefixTopicList + sid + ":" + strconv.Itoa(offset-topicsPerPage),
		})
	}
	if offset+topicsPerPage < len(topics) {
		nav = append(nav, models.InlineKeyboardButton{
			Text:         "▶",
			CallbackData: cbPrefixTopicList + sid + ":" + strconv.Itoa(offset+topicsPerPage),
		})
	}
	if len(nav) > 0 {
		rows = append(rows, nav)
	}
	if len(rows) == 0 {
		return text, nil
	}
	return text, &models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// removeSourceAny — единая точка /removesource: расширенный origin-aware
// remover (V2, приоритет) с откатом на legacy-контракт. Возвращает
// (found, static): static=true → источник origin='static', seam его НЕ
// отключил, ответ — подсказка про .env (план, подэтап B).
func (b *Bot) removeSourceAny(ctx context.Context, id int64) (found, static bool, err error) {
	if b.removeSourceV2 != nil {
		res, err := b.removeSourceV2(ctx, id)
		return res.Found, res.Static, err
	}
	found, err = b.removeSource(ctx, id)
	return found, false, err
}
