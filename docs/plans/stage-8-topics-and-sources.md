# Этап 8 — «Темы и источники»: план реализации

Ветка: `stage-8/topics-and-sources` (PR в `master`). Исполнитель: Agent `model: "opus"`.
Конвенции кода: `.claude/skills/go-telegram-bot-dev/SKILL.md` (обязателен к прочтению
перед стартом). План написан по фактическому коду на 2026-10-19 (коммит 5d63c6d).

---

## 1. Цель

Дать владельцу бота (single-user, `TELEGRAM_CHAT_ID`):

1. **Удобную регистрацию источников** тремя способами: форвард любого сообщения из
   канала/чата боту; команда `/dialogs` (список диалогов MTProto-аккаунта с
   inline-кнопками «добавить»); `/addsource` с пониманием t.me-ссылок, включая
   приватные `t.me/c/<id>/<msg>`.
2. **Темы**: источник принадлежит теме (привязка при добавлении и командами);
   LLM предлагает тему для нового источника, пользователь подтверждает кнопкой.
3. **Дайджест — одно сообщение-оглавление** (§4.11a, решение 2026-10-20):
   список тем с кнопками «открыть тему»; тема открывается edit'ом того же
   сообщения (один прогон = одно сообщение).
4. **Трёхуровневая детализация** у сообщения темы: L1 (сводка темы, кнопка
   «Подробнее») → L2 (саммари по каналам темы) → L3 (список исходных сообщений
   с переходами в чат/канал через t.me deep links, для приватных — `t.me/c/<id>/<msg>`).
   Данные прогона переживают рестарт (SQLite).

## 2. Ограничения и границы (что НЕ трогаем)

- **Не трогаем**: scheduler (один дневной слот, идемпотентность через meta
  `last_digest_date` — остаётся как есть); способ доставки (long-polling);
  LLM-клиентский транспорт (ручной net/http, retry/backoff — только новые методы
  поверх); MTProto-логин и шифрование сессии (`internal/mtproto/session.go`);
  Dockerfile/compose.
- **Без многопользовательности**: все новые апдейты (форварды, callback'и)
  принимаются только от owner (`authorized()`).
- **Тема — у источника, не у элемента.** Переназначение темы отдельному элементу —
  за пределами этапа (схема это позволит позже: `digest_items.source_id` уже несёт
  тему транзитивно).
- **Бот не читает приватные чаты сам**: чтение только через MTProto-сессию
  вторичного аккаунта. Если вторичный аккаунт не состоит в чате — регистрация
  падает с понятной ошибкой, источник не создаётся.
- **Приглашения (`t.me/+hash`, `/joinchat`) — вне этапа**: явный ответ «не умею».
- `pkg/` не появляется; новый код — в существующих пакетах `internal/*`
  (правило SKILL.md §1: заменять заглушки, а не плодить параллельные файлы).
- **Версии зависимостей не повышаем без нужды.** Проверено: `go-telegram/bot
  v1.22.0` уже имеет `models.Message.ForwardOrigin *MessageOrigin` и типы
  `MessageOriginChat`/`MessageOriginHiddenUser`/`MessageOriginChannel` в
  `models/reply.go` — bump не нужен (точные имена типов сверить при реализации).

## 3. Критерий готовности всего этапа

- Форвард сообщения из (а) публичного канала, где бот админ, (б) публичного канала,
  где бот не админ, (в) приватного канала/супергруппы — регистрирует источник
  правильного вида (`tg_botapi` / `tg_public` / `tg_mtproto`), повторный форвард —
  «уже добавлен (#N)».
- `/dialogs` показывает диалоги MTProto-аккаунта страницами, кнопка «добавить»
  регистрирует источник (включая приватные, через кеш access_hash).
- `/addsource https://t.me/<name>`, `t.me/<name>`, `/addsource t.me/c/<id>/<msg>`
  работают; http(s)-URL прочих хостов по-прежнему идут в `rss`.
- Новый managed-канал больше не крадёт посты соседнего канала (per-key buffer).
- `/topics`, `/newtopic`, `/settopic` работают; при добавлении источника LLM
  предлагает тему, подтверждение кнопкой привязывает её; без LLM-ключа —
  детерминированный offline-fallback без падения.
- Дневной прогон шлёт **одно сообщение-оглавление** (§4.11a): непустые темы
  списком с кнопками «открыть тему»; сбой темы = её пропуск в оглавлении
  (элементы не теряются — не seen), сбой доставки оглавления = слот не
  отмечается.
- Кнопка «Подробнее» на сообщении темы ведёт L1→L2→L3 и обратно через edit
  того же сообщения; L3 содержит рабочие ссылки на оригиналы; всё переживает
  рестарт процесса (данные в SQLite).
- Все новые пользовательские тексты экранированы под ParseMode HTML (урок
  хотфикса 4695043 — угловые скобки ломают доставку).
- `go build ./... && go vet ./... && gofmt -l . (пусто) && go test -race ./...`
  зелёные; golden-тесты рендера обновлены осознанно.

## 4. Ключевые архитектурные решения (приняты, не переоткрывать без нужды)

### 4.1. Тема хранится у источника; статические источники регистрируются в БД

Проблема: темы нужны всем источникам, но arxiv/hn/статические RSS/TG живут в
config и не имеют строк в `sources`. Решение: **при старте `app.Run` статические
источники upsert'ятся в `sources`** с `origin='static'` (колонка добавляется
миграцией v3). Config остаётся источником правды о существовании/включённости
статического источника; строка БД нужна только для привязки темы и единообразия
`/sources`. Upsert — по `UNIQUE(kind, ref)` (индекс создаётся миграцией v2).
`enabled` у static-строк игнорируется при сборке (решает config); `/removesource`
для static — запрещён с явным сообщением (править `.env`).

Альтернатива (привязка тем по имени источника-строке) отвергнута: имена не
уникальны и нестабильны.

### 4.2. Элементы знают свой источник: stamping при Collect

`feed.Item` получает два новых поля: `SourceID int64` и `TopicID int64`
(0 = «не проставлен» → pipeline относит к теме «Без темы»). Значения проставляет
обёртка `digest.taggedSource{inner feed.Source; sourceID, topicID int64}`, чей
`Collect` копирует элементы и stamp'ит поля. `DedupKey()` не меняется.
`Pipeline.SourceProvider` меняет сигнатуру: возвращает `[]digest.TrackedSource`
(`{Src feed.Source; SourceID, TopicID int64}`), а не `[]feed.Source`.

Альтернатива (маппинг по `Item.Source`-имени в pipeline) отвергнута: collision
имён между статическими и динамическими источниками реальны.

### 4.3. Drill-down переживает рестарт: таблицы `digests` + `digest_items`

Порядок записи в прогоне (на тему): **сначала** `InsertDigest` (delivered=0) →
получаем `digestID` → доставляем L1-сообщение с кнопкой, несущей `digestID` →
`MarkDelivered(digestID, items)` одной транзакцией: `delivered=1`, вставка строк
`digest_items`, `MarkSeen` элементов темы. Сбой доставки → строка остаётся
`delivered=0`, элементы не seen, следующий `/digest` их подберёт.

L2/L3-сообщения **не хранятся**: рендерятся по запросу из `digest_items`
(summary элемента — L2-строка; title+ссылка+snippet — L3-строка). Навигация —
через `editMessageText` того же сообщения (id берётся из CallbackQuery), поэтому
telegram message_id в БД не нужен.

### 4.4. callback_data: формат и лимит 64 байта

```
Открыть тему из оглавления (C):           "T:<digestID>"
Назад к оглавлению (C):                   "TOC:<runID>"     (runID = Unix-секунды старта прогона)
L1-кнопка «Подробнее» в L1-view темы (D): "L2:<digestID>"
L2-кнопка источника:                      "L3:<digestID>:<sourceID>"
L2-кнопка «Назад»:                        "L1:<digestID>"
L3-кнопка «Назад»:                        "L2:<digestID>"
Подтверждение темы при добавлении:        "ts:<sourceID>:<topicID>"
Выбрать другую тему:                      "tl:<sourceID>"
Остаться «Без темы»:                      "tn:<sourceID>"
Кнопка «добавить» в /dialogs:             "ad:<botAPIID>"   (int64, ≤19 цифр)
Пагинация /dialogs:                       "dp:<offset>"
```

Худший случай `L3:` + 19 + `:` + 19 = 42 байта < 64; `TOC:` + 19 = 23 байта
< 64. Все id — десятичные, без имён/строк. Парсер — `strings.CutPrefix` +
`strings.Cut`, не regex. Префиксы `T:` и `TOC:` не пересекаются (второй символ
различается), CutPrefix регистрозависим; в диспетчере `TOC:` проверяется раньше
`T:` (фиксированный порядок, подэтап C). Отдельный лимит — **текст** кнопки:
64 БАЙТА (урок A2 H1) — динамические лейблы режутся `TruncateButtonLabelTo`
по байтовому бюджету на границе рун.

### 4.5. Per-key ChannelBuffer (закрываем техдолг в A1)

Техдолг «один буфер на все tg_botapi-каналы» (см. комментарий в
`internal/app/dynamic_sources.go`) становится критичным, как только
managed-источников больше одного (форварды начнут добавлять их пачками).
Решение в A1: `ChannelBuffer` хранит `map[string][]feed.Item`; ключ —
`username` поста, а при пустом username (приватный чат, где бот админ) —
`strconv.FormatInt(post.Chat.ID, 10)`. `ManagedSource` получает ключ в
конструкторе. Лимит `maxBufferedItems=500` — на каждый ключ.

### 4.6. MTProto: числовые peer id, кеш access_hash, FloodWait

- Конверсия id: `internal/mtproto/helpers.go` —
  `func ToMTProtoID(botAPIID int64) (int64, bool)` (−100X → X; возвращает false
  для id не из диапазона channel/supergroup) и
  `func ToBotAPIID(mtprotoID int64) int64` (X → −100X).
- Канонический `ref` для `tg_mtproto` в `sources`: **строка в bot-API-форме
  `-100X`** для приватных (её же даёт форвард и `t.me/c/X/...`) или `@username`
  для публичных. Внутри mtproto-пакета −100 префикс снимается.
- access_hash для приватного peer нельзя получить из bot-API id напрямую — нужен
  кеш из `messages.getDialogs`. Новая таблица `mtproto_peers` (миграция v2) +
  интерфейс в пакете mtproto (чтобы не импортировать storage):

```go
// internal/mtproto/peers.go
type PeerInfo struct {
    MTProtoID int64  // X из -100X
    BotAPIID  int64  // -100X
    AccessHash int64
    Title     string
    Username  string // "" для приватных
    Kind      string // "channel" | "supergroup" | "chat" | "user"
}
type PeerCache interface {
    AccessHash(ctx context.Context, mtprotoID int64) (int64, bool, error)
    Store(ctx context.Context, p PeerInfo) error
}
```

  Реализацию поверх storage (`storagePeerCache`) собирает `app.Run` и инжектит в
  клиент: `NewClient(cfg MTProtoConfig, opts ...Option)` — опция
  `WithPeerCache(pc PeerCache)` (сигнатура NewClient меняется; вызовы в
  `cmd/bot/login.go` и `app.go` обновить; backward-compat не нужен, всё internal).
- Новые методы клиента:

```go
func (c *Client) GetDialogs(ctx context.Context, limit int) ([]PeerInfo, error)
    // messages.getDialogs, один вызов; каждый диалог нормализуется в PeerInfo
    // и сохраняется в PeerCache. FloodWait — через существующий middleware +
    // тот же паттерн backoff, что getHistoryWithRetry.

func (c *Client) GetHistoryByPeer(ctx context.Context, mtprotoID int64, limit int) ([]tg.Message, error)
    // access_hash из PeerCache; при промахе — один GetDialogs-рефреш и повтор;
    // при CHANNEL_INVALID после рефреша — ошибка «аккаунт не в этом чате».
```

- Новый источник: `internal/mtproto/peer_source.go` —
  `func NewPeerSource(name string, mtprotoID int64, limit int, client *Client) feed.Source`
  (Kind `tg_mtproto`, Source = `-100X`, URL элемента = deep link из кеша:
  username известен → `https://t.me/<user>/<msg>`, иначе `https://t.me/c/<X>/<msg>`).
  Существующий `messageToFeedItem` дополнить тем же построением URL.
- **FloodWait-дисциплина**: `/dialogs` — редкая ручная команда, но добавляем
  in-memory rate-limit: не чаще одного реального `GetDialogs` в 30 секунд
  (повторный вызов берёт данные из последнего успешного снапшота в памяти бота).
  Чтение приватных источников в дайджесте — как раньше, раз в сутки на источник,
  backoff+jitter уже в `client.go`. Триггер пересмотра: суммарный прогон > 10 мин.

### 4.7. Безопасность и авторизация новых апдейтов

- Форварды: обрабатываются только внутри уже прошедшего `authorized(u.Message.Chat.ID,
  b.chatID)` контура (форвард от постороннего чата игнорируется как и всё прочее).
- CallbackQuery: проверка `u.CallbackQuery.From.ID == b.chatID`. Для single-user
  приватного чата user id владельца == chat id (существующее допущение
  `TELEGRAM_CHAT_ID`); зафиксировать это в комментарии. Чужой callback →
  `AnswerCallbackQuery` с текстом «Не ваш бот» и без действий.
- `u.CallbackQuery.Message` может быть `InaccessibleMessage` (старое сообщение) —
  type-assert к `*models.Message`, при неудаче только ack.
- В текстах ответов никогда не выводим bot-токен/LLM-ключ/данные сессии
  (существующее правило; ошибки MTProto — только generic-текст + лог).

### 4.8. ParseMode HTML: систематическое экранирование

Хотфикс 4695043 чинил симптом (плейсхолдеры `<url|@channel>` в /start). В этапе 8
динамического текста (имена каналов, заголовки постов, LLM-саммари, темы) станет
много, поэтому: новый файл `internal/telegram/html.go` —
`func EscapeHTML(s string) string` (экранирует `&`, `<`, `>`), применяется **ко
всему динамическому тексту** в рендерах дайджеста/drill-down и в ответах команд,
выводящих имена/заголовки (/sources, /topics, подтверждения добавления). Разметка
создаётся только самим рендером: `<b>…</b>`, `<a href="URL">…</a>` (URL — только
http/https, проверяется перед вставкой). LLM-промпты просят **plain text без
HTML/маркдауна**; их вывод всё равно экранируется. Статические константы
`startText`/`helpText` остаются с квадратными скобками (уже безопасны).

### 4.9. Семантика частичного сбоя прогона (несколько тем)

Темы доставляются последовательно (одна LLM-вызов + одно сообщение на тему).
Сбой темы k: лог, тема k пропускается (элементы не seen → подберёт следующий
ручной `/digest`), цикл продолжается с темы k+1. `last_digest_date` ставится
планировщиком по завершении `Run` без фатальной ошибки — как сейчас; частичный
сбой не возвращает ошибку наверх (иначе слот «не выполнен» и при рестарте будет
дубль доставленных тем). Возврат `Run`: ошибка только если упали шаги до
доставки (resolve/collect-level) или не доставлена ни одна тема при их наличии.

### 4.10. LLM-предложение темы и структурированное саммари

Два новых маленьких интерфейса в `internal/llm` (вторая реализация —
client+offline — поэтому интерфейс оправдан правилом SKILL.md):

```go
// internal/llm/topic.go
type TopicSuggester interface {
    // SuggestTopic возвращает имя темы для нового источника: либо одно из
    // existing (точное совпадение), либо новое короткое имя (≤40 рун).
    // Пустая строка = «предложить нечего» (offline/ошибка — не фатально).
    SuggestTopic(ctx context.Context, existing []string, sample []feed.Item) (string, error)
}

// internal/llm/topicdigest.go
type ItemSummary struct { Index int; Summary string } // Index — 1-based позиция в срезе
type TopicSummary struct {
    Headline string        // L1-текст сообщения темы (plain text)
    Items    []ItemSummary // по одному на элемент, для L2
}
type TopicSummarizer interface {
    SummarizeTopic(ctx context.Context, topicName string, items []feed.Item) (TopicSummary, error)
}
```

- Client-реализация: отдельный system-prompt, требуем **строго JSON**
  `{"headline":"...","items":[{"index":1,"summary":"..."}]}`; парсинг —
  `encoding/json` после обрезки возможных ```-fence'ей. При непарсящемся ответе —
  **одна** попытка повторного запроса, затем fallback: `Headline` = результат
  существующего `Summarize`, `Items` = пусто (L2 тогда рендерится из заголовков
  элементов — деградация, не падение).
- Offline-реализация (`internal/llm/offline.go` — дописать): детерминированно.
  `SuggestTopic` → "" (без предложения). `SummarizeTopic` → Headline = первые 3
  заголовка списком; ItemSummary = первое предложение `Text` (или `Title`).
- В `app.Run`: если pipeline-суммаризатор offline — TopicSummarizer тоже offline.
  Добавить поле `TopicSuggest llm.TopicSuggester` в замыкание добавления источника.

### 4.11a. Модель доставки дайджеста — L2-оглавление одним сообщением (решение пользователя 2026-10-20)

Пользователь планирует **10+ тем**, поэтому триггер §7 «тем-сообщений слишком
много» считается **заранее активированным**, и митигация выбрана до старта
подэтапа C: **вместо N сообщений по темам — одно сообщение-оглавление** (список
тем с кнопками «открыть тему»); L1 каждой темы открывается по клику через edit
того же сообщения. Следствия:

- Подэтап C **передетализируется перед своим стартом** (отдельный проход
  планировщика): схема «InsertDigest на тему → доставка» заменяется на
  «N строк digests + одно оглавление с кнопками `T:<digestID>`»; формат
  callback_data §4.4 пополняется префиксом оглавления (укладывается в 64 байта:
  `T:` + 19 + опциональная пагинация).
- C получает зависимость от callback-каркаса и edit-механики (каркас появляется
  в A2, полноценный диспетчер — в D); порядок подэтапов A1 → A2 → B → C → D
  остаётся валидным, но D-механика `EditMessage` нужна уже в C — при
  перепланинге C решить, тянуть ли `EditMessage`/`AnswerCallback` в C или
  поменять D и C местами частично.
- Семантика частичного сбоя §4.9 сохраняется по духу: сбой темы = пропуск темы
  в оглавлении (элементы не seen), сбой доставки оглавления = ошибка Run, слот
  не отмечается выполненным.

Решение принято пользователем 2026-10-20 — не переоткрывать без его слова.
Перепланирование выполнено 2026-10-20 — раздел «Подэтап C» актуализирован
(решения планировщика зафиксированы в самом разделе: формат восстановления
оглавления `TOC:<runID>`, колонка `digests.run_id` в миграции v4, отказ от
нового telegram-транспорта в C в пользу механик A2/B, разбиение на PR C1→C2).

### 4.11. Поток добавления источника с темой (подэтап B)

Единая точка входа — существующее замыкание `addSourceFn` в `app.go`, расширенное
до `addSourceWithTopicFn(ctx, kind, ref string, topicID int64) (id int64, fresh int, err error)`
(`topicID=0` → «Без темы»). После успешного добавления любого источника бот:

1. Берёт `sample` (элементы trial-collect; для `tg_botapi` — пусто) и
   `existing = ListTopics`.
2. `SuggestTopic` (таймаут 15с; ошибка → без предложения).
3. Если предложено имя: `CreateTopic(name)` **сразу** (идемпотентно по UNIQUE
   name), ответное сообщение «Источник #N добавлен. Предложенная тема: X» с
   кнопками `ts:<N>:<topicID>` / `tl:<N>` / `tn:<N>`.
4. `ts` → `SetSourceTopic`; `tl` → edit в список тем (кнопки `ts:<N>:<id>` по
   страницам до 8 штук, `dp`-пагинация переиспользуется); `tn` → ничего (остался
   «Без темы»), ack «Оставлено без темы».

Неподтверждённая suggested-тема остаётся в `topics` пустой — принимаемый
мусорный сценарий (чистится вручную `/topics`; авто-prune — техдолг, см. §7).

---

## 5. Подэтапы (каждый — отдельный reviewable PR, зелёная сборка+тесты)

Порядок A1 → A2 → B → C → D обоснован: A2 (`/dialogs`) переиспользует кеш пиров
и регистрацию из A1; B даёт `topic_id`, без которого C не может группировать;
C пишет `digest_items`, без которых D нечего рендерить. A1 вынесен раньше A2 и
объединяет «форвард + t.me-ссылки + per-key buffer + MTProto-пиры», потому что
все четыре упираются в одну схему (`mtproto_peers`, UNIQUE-индекс) и одну
механику регистрации; дробление дало бы три PR, трогающих одни файлы.

---

### Подэтап A1 — регистрация источников: форвард, t.me-ссылки, приватные пиры

**Файлы (изменяемые/новые):**

| Файл | Что |
|---|---|
| `internal/storage/migrate.go` | миграция v2 (см. ниже) |
| `internal/storage/peers.go` (новый) | CRUD `mtproto_peers` + `AddSourceIfNotExists` |
| `internal/storage/store.go` | методы в интерфейс `Store` |
| `internal/telegram/buffer.go` | per-key ChannelBuffer |
| `internal/telegram/botapi_source.go` | ManagedSource с ключом |
| `internal/telegram/links.go` (новый) | парсер t.me-ссылок |
| `internal/telegram/source_admin.go` | detectKind: t.me до rss-ветки |
| `internal/telegram/forward.go` (новый) | обработка форварда |
| `internal/telegram/router.go` | ветка ForwardOrigin; тексты /start,/help |
| `internal/telegram/bot.go` | опция `WithForwardRegistrar` |
| `internal/telegram/html.go` (новый) | `EscapeHTML` (используется с A1) |
| `internal/mtproto/helpers.go` | конверторы id |
| `internal/mtproto/peers.go` (новый) | `PeerInfo`, `PeerCache` |
| `internal/mtproto/client.go` | `GetDialogs`, `GetHistoryByPeer`, `WithPeerCache` |
| `internal/mtproto/peer_source.go` (новый) | `NewPeerSource` |
| `internal/mtproto/source.go` | URL/deep link в `messageToFeedItem` |
| `internal/app/app.go` | сборка: peer cache, MTProto-клиент **создаётся при непустых MTPROTO_APP_ID/HASH даже без MTPROTO_CHANNELS** (нужен для приватных источников из БД); dynamic `tg_mtproto` источники; форвард-регистрация |
| `internal/app/dynamic_sources.go` | ветка `tg_mtproto` (ref `-100X` → NewPeerSource, `@user` → NewChannelSource), per-key буфер |
| `internal/storage/sources.go` | `AddSourceIfNotExists` |

**Миграция v2** (добавляется в `migrations` в конец, никогда не правим v1):

```sql
CREATE TABLE IF NOT EXISTS mtproto_peers (
    peer_id     INTEGER PRIMARY KEY,          -- MTProto id (X из -100X)
    access_hash INTEGER NOT NULL,
    title       TEXT NOT NULL DEFAULT '',
    username    TEXT NOT NULL DEFAULT '',
    updated_at  TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS sources_kind_ref ON sources(kind, ref);
```

Перед UNIQUE-индексом проверить дамп текущей БД на дубликаты `(kind,ref)`; если
дедупликация уже гарантирована кодом (`addSourceFn` проверяет существующие) —
индекс безопасен. Если на прод-БД индекс не ставится из-за дублей — миграция
должна упасть явно (не глотать), чистка дублей отдельным ручным запросом.

**Новые хранилищные методы (сигнатуры):**

```go
// storage.Store (интерфейс пополняется):
AddSourceIfNotExists(ctx context.Context, kind, ref string) (id int64, created bool, err error)
UpsertPeer(ctx context.Context, peerID, accessHash int64, title, username string) error
GetPeer(ctx context.Context, peerID int64) (p Peer, ok bool, err error)   // storage.Peer — mirror mtproto.PeerInfo без Kind
```

`AddSourceIfNotExists`: `INSERT ... ON CONFLICT(kind,ref) DO NOTHING` + выборка id
(одна транзакция; race двух параллельных добавлений закрывает UNIQUE-индекс).

**Парсер ссылок** (`internal/telegram/links.go`):

```go
type TmeLink struct {
    Kind     string // "public" | "private" | "unsupported"
    Username string // Kind=="public": без '@'
    BotAPIID int64  // Kind=="private": -100X из t.me/c/X
}
func ParseTmeLink(raw string) (TmeLink, bool) // bool=false — не t.me-ссылка
```

Принимает: `t.me/<u>`, `telegram.me/<u>`, `https://t.me/<u>`, `t.me/s/<u>`
(префикс `s` снимается), `t.me/c/<X>[/<msg>]` → private, `X` → `-100X`.
`t.me/+…`, `/joinchat`, `/proxy`, `/share`, `/addstickers` и т.п. → Kind
"unsupported" (сообщение «пригласительные ссылки не поддерживаются»). В
`detectKind`: проверка `ParseTmeLink` **до** http/https→rss ветки; public →
существующая логика isBotAdmin; private → kind `tg_mtproto`, ref `-100X`.

**Обработка форварда** (`internal/telegram/forward.go`):

```go
// ForwardRegistrar регистрирует источник по форварду; возвращает текст ответа.
type ForwardRegistrar func(ctx context.Context, origin ForwardOrigin) (reply string)
type ForwardOrigin struct {
    BotAPIChatID int64  // id чата-источника (канал/супергруппа), -100X
    Username     string // "" для приватных
    Title        string // отображаемое имя
}
```

Логика в app-замыкании (не в telegram-пакете — там только извлечение):
1. Из `u.Message.ForwardOrigin` (type-switch по `*models.MessageOriginChannel` и
   `*models.MessageOriginChat`; `MessageOriginHiddenUser`/`MessageOriginUser` →
   отказ «могу добавлять только каналы и чаты, не личные переписки»).
2. `Username != ""` → публичный: `isBotAdmin` → `tg_botapi` (ref=username, ключ
   буфера=username), иначе `tg_public`.
3. `Username == ""` → приватный: если MTProto не сконфигурирован → ответ
   «приватные чаты требуют настроенного MTProto (MTPROTO_APP_ID/HASH)»; иначе
   kind `tg_mtproto`, ref = `-100X`. Валидация: пробный
   `GetHistoryByPeer(ctx, X, limit=1)` с таймаутом 25с (FloodWait внутри клиента)
   — успех ⇒ `AddSourceIfNotExists`; ошибка «не в чате» ⇒ понятный отказ.
   Дополнительно: если бот админ в приватном чате (проверка `getChatMember` по
   числовому id) — предпочесть `tg_botapi` с ref=`-100X` и ключом буфера = chat id
   (бот сам читает посты, MTProto не нужен). Решение принимается по факту
   админства, порядок проверок: botapi-админ → mtproto.
4. Ответ — с `EscapeHTML(title)` и пометкой вида источника; «уже добавлен (#N)»
   при `created==false`.

**ChannelBuffer per-key**: `Push(key string, item feed.Item)`,
`Drain(key string) []feed.Item`; `map[string][]feed.Item`; кап 500/ключ.
`NewManagedSource(name, key string, buf *ChannelBuffer)`. В `handleUpdate` ключ:
`post.Chat.Username`, иначе `strconv.FormatInt(post.Chat.ID, 10)`.

**Тексты /start и /help (A1):** добавить строки
«Перешлите любое сообщение из канала или чата — добавлю его как источник» и
«/addsource понимает t.me-ссылки, включая приватные t.me/c/…». Плейсхолдеры —
только в квадратных скобках.

**Тест-кейсы A1:**
- `links_test.go` (table-driven): публичная/приватная/s-префикс/joinchat(unsupported)/мусор/без схемы/лишний путь.
- `buffer_test.go`: два ключа — Drain одного не трогает другой; кап на ключ.
- `router_test.go`: форвард от owner → registrar вызван с верным ForwardOrigin; форвард от чужого чата → проигнорирован; hidden-user forward → отказ; не-форвард сообщение → прежние команды.
- `migrate_test`/`sources_test.go`: v2 применяется поверх v1 (user_version=2, идемпотентно при повторном Open); `AddSourceIfNotExists` — второй вызов `created=false`, тот же id; UNIQUE-индекс существует.
- `client_test`/`peers_test` в mtproto: конверторы id (−100X↔X, граничные); `storagePeerCache` roundtrip; `GetHistoryByPeer` без кеша → ошибка с понятным текстом (без сети, на fake/injected API-слое — если gotd-фейк дорог, вынести resolve-логику в тестируемую функцию `resolveInputPeer(cache, mtprotoID)`).
- `dynamic_sources_test.go`: строка `tg_mtproto` с ref `-100X` → PeerSource; `@user` → ChannelSource; disabled пропускаются.
- Интеграция (фейки, temp SQLite): форвард-регистрация приватного канала с фейковым mtproto-клиентом (интерфейс не вводим — app-замыкание тестируется через подмену `ForwardRegistrar` в telegram-пакете; сетевой слой вне юнитов).

**Итог A1 (реализован 2026-10-20, сдан одним PR — решение пользователя):**

- Прогон: build/vet/gofmt чистые, `go test -race` 8/8 пакетов ok. Ревью (Opus):
  1 high + 5 medium найдено и закрыто в цикле фиксов, low — частично техдолг.
- **Подтверждённые отклонения от плана** (не переоткрывать):
  - `AddSourceIfNotExists` → `(id, created, reactivated bool, err)`: повторное
    добавление disabled-строки РЕАКТИВИРУЕТ её («✅ #N снова включён») — фикс
    регрессии H1 против master (старая проверка дублей учитывала только enabled).
  - `ForwardOrigin` — struct с полями Type/BotAPIChatID/Username/Title/**BotIsAdmin**
    (admin-чек в telegram-пакете: у app нет доступа к bot.Bot); не интерфейс.
  - `PeerCache` расширен `Get()` (username для deep link); `storagePeerCache`
    живёт в `internal/app` (mtproto не импортирует storage); в кеш пишутся
    только channel/supergroup (id-коллизии с basic chat/user).
  - CHANNEL_INVALID/CHANNEL_PRIVATE → sentinel `errPeerStale` → один
    `refreshPeerCache` + повтор (M1, митигация §7); `refreshPeerCache` —
    in-memory rate-limit 30с, только успешные рефреши (M2).
  - `messageToFeedItem(msg, source, url)` — URL параметризован (L8).
  - `ParseTmeLink` отклоняет `t.me/c/X` при X ≥ 1e12 (симметрично ToMTProtoID, L3).
  - Замыкания добавления/форварда извлечены из `Run` в `newSourceAdder`/
    `newForwardRegistrar` (тестируемость); test-seam `deliverFn` в telegram.Bot.
    Плановый интеграционный кейс «форвард с фейковым mtproto-клиентом»
    **осознанно не реализован**: addMTProtoSource сетевой, вне юнитов;
    registerForward покрыт тестами на ветвях без сети (register_forward_test.go).
- **Техдолг A1** (триггеры пересмотра):
  1. Число ключей ChannelBuffer не ограничено (L5) — single-user, ущерб = память;
     триггер: бот админ во многих чужих каналах.
  2. `storage.AddSource` — мёртвый production-код, используется только
     migrate_test (L6); удалить в B/C, если не понадобится.
  3. Валидация приватного источника считает только `*tg.Message` (L7): новый
     канал с одним служебным сообщением будет отклонён; триггер: отказ
     регистрации валидного чата.
  4. Форвард из обычной группы (MessageOriginChat, id не −100…) при боте-админе
     создаёт tg_botapi-источник, который ничего не собирает (handleUpdate
     буферизует только ChannelPost); чинить в A2/B при встрече.
  5. `addMTProtoSource` без юнитов (сеть) — покрыть, если появится seam.
- **Перед деплоем на прод**: проверить БД VPS на дубликаты `(kind,ref)` —
  миграция v2 (UNIQUE-индекс) на дублях упадёт явно; дедуп-скрипт —
  `docs/handoff/2026-10-20-stage-8-plan-gate.md`.

---

### Подэтап A2 — `/dialogs`

**Файлы:** `internal/telegram/dialogs.go` (новый), `router.go` (команда +
callback'и `ad:`/`dp:`), `bot.go` (опции `WithDialogLister`,
`WithCallbackHandler` — каркас callback-обработки появляется здесь, D его
расширяет), `app.go` (замыкание поверх `Client.GetDialogs` + регистрация
выбранного пира как `tg_mtproto`-источника), `html.go` (используется для title).

**Механика:**
- `/dialogs` → rate-limit 30с (§4.6) → `GetDialogs(ctx, limit=100)` → snапшот в
  памяти (`[]mtproto.PeerInfo`) → страницы по 8: строка
  `• <title> [@username] (channel)` + кнопка «Добавить» `ad:<botAPIID>` +
  навигация `dp:<offset>` («◀ / ▶»). `AnswerCallbackQuery` обязателен на каждый
  callback. Кнопка `ad:` → тот же путь валидации, что для форварда приватного
  (GetHistoryByPeer limit=1) → `AddSourceIfNotExists("tg_mtproto", "-100X")` →
  edit сообщения «✅ Добавлен #N» (или «уже добавлен»). Публичные диалоги с
  username → регистрация как `tg_public`/`tg_botapi` по той же логике admin-чека.
- Диалоги вида `user` (личные переписки) и `chat` (обычные группы) — кнопка без
  «добавить»/отказ: дайджест личных чатов вне этапа (граница!). Только
  channel/supergroup.
- MTProto не сконфигурирован → «/dialogs требует настроенный MTProto-аккаунт».

**Тексты /start,/help (A2):** строка «/dialogs — список моих Telegram-диалогов,
добавление кнопкой».

**Тест-кейсы A2:** пейджинг-рендер (0/1/8/9 диалогов — golden); callback `ad:`
от owner/не-owner; rate-limit (второй вызов до 30с берёт снапшот, `GetDialogs`
вызван один раз — считаем вызовы фейка); отказ для user-диалога. Фейк:
`dialogsFetcher func(ctx, limit) ([]mtproto.PeerInfo, error)` в telegram.Bot —
тестируем router без gotd.

**Итог A2 (реализован 2026-10-20, сдан одним PR):**

- Прогон: build/vet/gofmt чистые, `go test -race` 8/8 пакетов ok. Ревью (Opus):
  1 high + 1 medium закрыты фиксами до коммита; low — частично фиксы, частично
  техдолг. Тестовый агент: все 5 плановых тест-кейсов покрыты (10 тест-функций,
  17 подтестов, 0 skip).
- **Подтверждённые отклонения от плана** (не переоткрывать):
  - Регистрация выбранного пира переиспользует `ForwardRegistrar` из A1 целиком
    (admin-чек в telegram-пакете → tg_botapi/tg_public/tg_mtproto с trial-
    валидацией); edit после «Добавить» показывает тексты регистратора
    («✅ Источник #N добавлен…», «ℹ️ уже добавлен», «✅ снова включён»), а не
    отдельный плановый «✅ Добавлен #N» — без дублирования валидации.
  - В go-telegram/bot v1.22 `MaybeInaccessibleMessage.Message` — конкретное
    поле `*Message`, не интерфейс: плановский type-assert заменён nil-проверкой
    (`callbackMessage`), поведение «только ack» сохранено.
  - user/chat-пиры имеют `BotAPIID=0` (нормализация mtproto пишет bot-API-id
    только channel/supergroup): crafted-callback `ad:0` находит первый такой
    пир и отклоняется kind-гейтом до регистрации; кнопка с id=0 не рендерится.
  - Текст inline-кнопки Bot API ограничивает **64 байтами** (H1 ревью):
    `truncateButtonLabel` режет по байтовому бюджету 43 байта (64 − «Добавить: »
    − «…») с границей рун; тест на кириллический 30-рунный и emoji-заголовок.
  - Вызов `GetDialogs` под мьютексом снапшота ограничен `dialogsFetchTimeout`
    30с (M1 ревью): FloodWait-сны внутри клиента иначе держали бы замок
    минутами, блокируя параллельные /dialogs, ad:, dp:.
  - `renderDialogsPage` выравнивает offset кратно 8 (L1): крафтовый `dp:3`
    больше не даёт тупиковую «◀ dp:-5»; для мусорного `dp:` — отдельный текст
    `dialogBadPageText` (L2); `p.Kind` через EscapeHTML (L3, защита от будущей
    регрессии нормализации).
- **Техдолг A2** (триггеры пересмотра):
  1. После истечения 30-с окна ошибка рефреша не фолбэчится на старый снапшот
     (L4) — пользователь видит «❌ Не удалось получить список диалогов»;
     триггер: жалобы на UX при нестабильной сети/FloodWait.
  2. Повторный идентичный edit («message is not modified», двойной тап)
     логируется как Error (L5); триггер: шум в логах VPS — детектировать код
     ошибки и понизить до Debug.
  3. Не покрыты тестами: error-ветки `dialogLister`/`forwardRegistrar==nil`,
     порядок «ack раньше edit» (только счётчики), неизвестный префикс при
     живом `callbackHandler`; триггер: подэтап D меняет обработку callback'ов.
- Каркас `WithCallbackHandler` заведён, но в app.go не проводится — Stage D
  wiring'ит его на L1/L2/L3 и ts/tl/tn; при nil неизвестный префикс получает
  пустой ack (контракт «каждый callback отвечен» не нарушается).

---

### Подэтап B — темы: схема, команды, LLM-предложение

**Файлы:** `migrate.go` (v3), `storage/topics.go` (новый), `store.go` (интерфейс),
`storage/sources.go` (`topic_id`, `origin` в `Source`), `llm/topic.go` (новый),
`llm/client.go` + `llm/offline.go` (реализации `SuggestTopic`),
`telegram/topics_admin.go` (новый: /topics,/newtopic,/settopic + callback'и
`ts:/tl:/tn:`), `router.go`, `bot.go` (опция `WithTopicSuggestion` — замыкание
«после добавления предложи тему»), `app.go` (регистрация статических источников
в БД при старте; `addSourceFn` → версия с topicID; сборка suggested-потока),
`source_admin.go` (ответ «добавлен» дополняется кнопками темы).

**Миграция v3:**

```sql
CREATE TABLE IF NOT EXISTS topics (
    id         INTEGER PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL
);
INSERT OR IGNORE INTO topics(id, name, created_at) VALUES (1, 'Без темы', datetime('now'));
ALTER TABLE sources ADD COLUMN origin   TEXT NOT NULL DEFAULT 'dynamic';
ALTER TABLE sources ADD COLUMN topic_id INTEGER REFERENCES topics(id);
```

`topic_id NULL` ≡ тема 1 («Без темы») — запросы читают
`COALESCE(topic_id, 1)`.

**Новые методы Store:**

```go
ListTopics(ctx context.Context) ([]Topic, error)                 // Topic{ID, Name, SourceCount int}
CreateTopic(ctx context.Context, name string) (id int64, err error) // идемпотентно по имени
SetSourceTopic(ctx context.Context, sourceID, topicID int64) (found bool, err error)
UpsertStaticSource(ctx context.Context, kind, ref string) error  // origin='static', ON CONFLICT не трогает topic_id
```

`Source` пополняется полями `Origin string`, `TopicID int64`.

**Команды:** `/topics` — список тем с источниками (id, имя, count, по строке на
источник); `/newtopic <имя>` — создание (имя ≤40 рун, UNIQUE, экранируется в
ответе); `/settopic <sourceID> <topicID|имя темы>` — привязка; для static-источков
разрешена (это главный способ дать тему arxiv/hn). `/removesource` для static →
«статические источники управляются через .env».

**Регистрация статических источников при старте** (`app.Run`, до sourceProvider):
для каждого static-источника вывести `(kind, ref)`: RSS/arXiv → (`rss`|`arxiv`?)
Внимание: `sources.kind` сегодня ∈ {rss,tg_botapi,tg_public} + документированные
static-only {arxiv,hn,tg_mtproto}. Для строк БД разрешаем kind ∈ полное
множество feed.Kind: `arxiv`, `hn` (ref = "frontpage"), `rss`, `tg_botapi`,
`tg_public`, `tg_mtproto`. `buildDynamicSources` не должен строить источники для
static-строк (иначе дубли) — фильтровать `origin='static'` там; статический слой
продолжает строиться из config как сейчас, но sourceProvider проставляет
SourceID/TopicID по совпадению `(kind,ref)` с БД-строкой (map из того же
`ListSources`, один запрос на прогон).

**Suggested-поток** — по §4.11. Кнопки шлются **вместо** текущего текстового
«✅ Источник #N добавлен» (сообщение одно: текст + inline-клавиатура).

**Тексты /start,/help (B):** строки «/topics — темы и их источники»,
«/newtopic <имя> — новая тема», «/settopic <id источника> <id темы> — привязать»;
упоминание, что дайджест приходит по темам (заработает в C — добавляем строку
только в C, чтобы не обещать нерабочее; в B — только команды управления).

**Тест-кейсы B:** миграция v3 поверх v2 (user_version=3, старые строки получают
origin='dynamic', topic NULL → COALESCE 1); CreateTopic идемпотентен;
SetSourceTopic found/not-found; static upsert не затирает topic_id при повторном
старте; llm client SuggestTopic — httptest: валидный JSON, JSON в fence'ах,
мусор→две попытки→ошибка, ответ не из existing→принимается как новое имя;
offline SuggestTopic == ""; роутер: /topics рендер (golden), /newtopic с
экранированием `<b>` в имени, /settopic parse-ошибки; suggested-callback'и
ts/tl/tn от owner и не-owner; addSource без LLM-ключа не падает и не шлёт кнопки
предложения.

**Итог B (реализован 2026-10-20, сдан одним PR):**

- Прогон: build/vet/gofmt чистые, `go test -race` 8/8 пакетов ok; все 18
  плановых тест-кейсов покрыты (85 новых RUN/PASS, 0 skip) + сверх плана
  (64-байтовые лейблы кнопок, tl-пагинация goldens, static-фильтры, pruning).
  Ревью (Opus): 0 high, 4 medium — M1/M3/M4 закрыты фиксами, M2 принят как
  плановая синхронность; 6 low — L1/L2 исправлены, L3–L6 техдолг/приняты.
- **Подтверждённые отклонения от плана** (не переоткрывать):
  - Пагинация пикера тем — свой префикс `tl:<sourceID>:<offset>`, НЕ `dp:`
    (dp: перерисовывает диалоги A2); механика (кламп, 8/страница, ◀/▶) та же.
  - Suggested-кнопки только при `SourceID != 0` (created/reactivated);
    «уже добавлен»/ошибки → старый ответ без кнопок.
  - kind/ref статиков: rss/arxiv = (вид по «arxiv.org» в URL, полный URL),
    hn = (hn, "frontpage"), tg_* = username без @; `MTPROTO_CHANNELS` без
    mtClient НЕ регистрируются, но в pruning keep-set входят (см. ниже).
  - CreateTopic — INSERT OR IGNORE + SELECT в транзакции (переносимее RETURNING).
  - Sample для SuggestTopic = trial-элементы пути добавления (фикс M3):
    `SourceAddResult{ID,Fresh,Sample}` / `ForwardResult.Sample` / seam
    `TopicSuggester(+sample []feed.Item)`; tg_botapi — всегда пусто (Collect
    дренировал бы production channelBuf), tg_mtproto — 1 элемент из
    валидационного GetHistoryByPeer через `mtproto.TrialItem` (второй запрос
    не делается); `collectTopicSample` удалён.
  - `WithTopicSuggestion` проводится только для `*llm.Client` (фикс M1):
    offline — опция не проводится, ноль дополнительных сетевых вызовов.
  - Pruning осиротевших static-строк при старте (`PruneStaticSources`, фикс
    M4); keep-set включает `MTPROTO_CHANNELS` даже при mtClient==nil —
    деградный запуск не теряет привязку topic_id (правка диспетчера сверх
    рекомендации ревью).
  - Legacy-опции (WithSourceRemover/WithForwardRegistrar/старый sourceAdder)
    в telegram сохранены для совместимости, app переведён на V2.
- **Техдолг B** (триггеры пересмотра):
  1. M2 принят: suggested-поток синхронен в обработчике апдейта (план §4.11 —
     одно сообщение «текст+кнопки»); худший случай с LLM-ключом ≈ trial 20с +
     suggest 15с, апдейты в это время стоят; триггер: таймауты spinner/кнопок
     в живом использовании → асинхронный вариант (базовый ответ сразу, кнопки
     edit'ом из горутины).
  2. L3: TOCTOU newTopicCreator (ListTopics→CreateTopic не атомарны) —
     single-user, задокументирован в коде.
  3. L4: extractContent дублирует инлайн-парсинг ответа в Summarize —
     унифицировать при следующей правке llm (в C).
  4. L5: `tn:`-callback не валидирует формат sourceID — безвредно (ack-only,
     состояние не меняется); прикрыть в D при общем диспетчере.
  5. Неподтверждённые suggested-темы остаются пустыми строками (§4.11 —
     принимаемый сценарий); авто-prune пустых тем — триггер: >5 пустых тем
     в /topics.
- Граница C соблюдена: digest.Pipeline / feed.Item / SourceProvider не
  тронуты; TopicSummarizer/topicdigest.go не создавались.

---

### Подэтап C — дайджест по темам: оглавление одним сообщением

> **Раздел перепланирован 2026-10-20** по §4.11a (решение пользователя, не
> переоткрывать): **одно сообщение-оглавление вместо N сообщений по темам**;
> каждая тема открывается кнопкой через edit того же сообщения. Ниже — конечная
> спецификация для холодного Opus-исполнителя (самодостаточная); исходный
> замысел «сообщение на тему + RenderTopic + кнопка L2: при доставке» полностью
> заменён. Вопрос §4.11a «тянуть ли EditMessage/AnswerCallback в C» решён:
> **нового telegram-транспорта в C нет** — используются механики A2/B
> (`handleCallbackQuery`-цепочка CutPrefix, `answerCallback`,
> `editCallbackMessage`, `callbackMessage`, seams `sendFn/ackFn/editFn`);
> абстракция `CallbackResult` и единый диспетчер остаются в D (решение
> планировщика).

**Граница C/D (зафиксирована, не разъезжается; раздел D не правится):**

- **В C:** миграция v4; `storage/digests.go` (InsertDigest/MarkDelivered/
  GetDigest/ListDigestItems/ListRunDigests); удаление `SaveDigest`; поля
  `feed.Item.SourceID/TopicID` + `digest.TrackedSource`/`taggedSource` (§4.2);
  перепись `Pipeline.Run` (группировка по темам → оглавление → MarkDelivered
  по темам); `llm.TopicSummarizer` (client + offline, §4.10); рендеры
  `RenderTopicBody`/`RenderTopicL1`/`RenderTOC`/`TOCButtons`; telegram:
  `InlineButton`, `DeliverWithButtons`, callback'и `T:` (L1-view темы) и
  `TOC:` (назад к оглавлению), seam `WithDigestViewer`; wiring в app; строка
  /start,/help про дайджест по темам.
- **В D:** префиксы `L1:`/`L2:`/`L3:`, рендеры L2 (по источникам) и L3 (ссылки
  на оригиналы), `ListDigestItemsBySource`, `DeepLink`, расширение диспетчера до
  `CallbackResult`. Выборки `GetDigest`/`ListDigestItems`, перечисленные в
  разделе D, **создаются в C**; D добавляет только `ListDigestItemsBySource`.
- **Общий хелпер (дублирование L1-рендера запрещено):** `L1:<digestID>`-
  восстановление в D ОБЯЗАНО переиспользовать `digest.RenderTopicL1` из C; D
  лишь добавляет в кнопочный ряд кнопку «Подробнее →» (`L2:<digestID>`) рядом с
  «◀ К оглавлению» (композиция кнопок — слой D/telegram, сигнатуру хелпера не
  менять).

**Модель доставки — один прогон Run (§4.11a):**

1. `SourceProvider` → `[]TrackedSource`; collect через обёртки `taggedSource`
   (stamp'ят `SourceID/TopicID`, §4.2); частичные ошибки collect — Warn, не
   фатальны (как сейчас).
2. `FilterUnseen`; **нет свежих элементов → log + `return nil`**: ни
   оглавления, ни строк digests — семантика текущего шага 3 Run сохраняется.
3. `ListTopics` один раз → map `id→name`; группировка fresh-элементов по
   `TopicID` (**0 → 1 «Без темы»**); порядок тем — **id asc** (`slices.Sort`);
   порядок элементов внутри темы — порядок collect (стабильный, никакой
   map-итерации). Имя темы отсутствует в map (FK не enforcement'ится) →
   `Тема #<id>` (решение планировщика).
4. `runID := now.Unix()` (now — через `p.Now`; один на весь прогон). Решение
   планировщика: коллизия двух прогонов в одну секунду при single-user и
   LLM-вызовах практически невозможна, в тестах Now фиксирован.
5. На каждую тему (последовательный цикл):
   a. `SummarizeTopic(topicName, items)` (§4.10). Ошибка → `log.Error`,
      **тема пропускается в оглавлении** (её элементы НЕ seen — подберёт
      следующий прогон; строка digests не создаётся), цикл продолжается.
   b. `body := RenderTopicBody(topicName, ts.Headline, len(items), now)`;
      `InsertDigest(ctx, runID, topicID, body, len(items))` → `digestID`
      (delivered=0). Ошибка → log, пропуск темы, continue.
   c. `pending += {digestID, []DigestItem}` (строки — `storage.NewDigestItem`,
      summary сопоставляется по `ItemSummary.Index`); `entries += TOCEntry`.
6. **Все темы упали** (fresh есть, entries пуст) → `Run` возвращает ошибку —
   сохранена семантика текущего pipeline (сегодня fresh + ошибка summarize =
   ошибка Run) и §4.9 «не доставлена ни одна тема при их наличии». Слот
   планировщика не отмечается (runJob вызывает setSlot только при успехе —
   существующее поведение, не трогать).
7. `text := RenderTOC(entries, date)`, `buttons := TOCButtons(entries)`,
   `Poster.DeliverWithButtons(ctx, ChatID, text, buttons)`. **Ошибка доставки →
   `Run` возвращает ошибку** (`deliver toc: %w`): все строки delivered=0,
   элементы не seen, слот не отмечен; повтор (/digest или catch-up
   планировщика) собирает новое оглавление.
8. После успешной доставки — на каждую тему `MarkDelivered(digestID, items)`
   (одна транзакция: delivered=1 + digest_items + seen_items). Ошибка здесь →
   **только `log.Error`** (оглавление уже у пользователя; элементы темы
   останутся unseen и задвоятся в следующем прогоне — принимаемый сценарий,
   аналог текущего шага 7 Run). `Run` возвращает nil — частичный сбой наверх не
   уходит (§4.9).
9. `log.Info("digest delivered", "topics", len(entries), "run_id", runID)`.

Инвариант §4.3 **«доставка гейтит персистенцию»** сохранён: InsertDigest
(delivered=0) → доставка (оглавление с кнопками `T:<digestID>`) →
MarkDelivered; «L1-сообщение с кнопкой» из §4.3 в новой модели — кнопка
оглавления. Формулировки §4.9 про «одно сообщение на тему» устарели —
каноническая семантика сбоя для C — шаги 5–8 выше (по духу §4.11a).

**Рестарт (rest-семантика, §4.3):** сообщение-оглавление и его кнопки живут в
чате Telegram; данные для клика `T:`/`TOC:` берутся **из SQLite, не из памяти**
— после рестарта бота `T:<digestID>` → `GetDigest` + `ListDigestItems` →
`RenderTopicL1`; `TOC:<runID>` → `ListRunDigests` + `ListTopics` → `RenderTOC`.
In-memory снапшотов прогона telegram-пакет не хранит. Крайний случай: crash
между шагами 7 и 8 → строки delivered=0 без digest_items: `T:` рендерит body из
`digests` без items-блока (решение планировщика: допустимо), элементы подбирает
следующий прогон в новое оглавление; старое оглавление не обновляется
(single-user, принимаем).

**Callback-данные C (внесены в §4.4):** открытие темы `T:<digestID>`; назад к
оглавлению `TOC:<runID>`. Решение планировщика: в `TOC:` несём runID, а не
digestID, — оглавление это view всего прогона, а digestID потребовал бы
индиректа через GetDigest и ломался бы на crash-кейсе шага 7–8. Худший случай:
`TOC:` + 19 цифр = 23 байта < 64. Префиксы `T:`/`TOC:` не пересекаются (второй
символ различается), CutPrefix регистрозависим; порядок фиксирован: `TOC:`
проверяется раньше `T:` (читаемость). Конфликтов с ad:/dp:/ts:/tl:/tn: и
будущими L1:/L2:/L3: нет.

**Миграция v4** (append в конец `migrations` в `internal/storage/migrate.go`;
v1–v3 не правим никогда):

```sql
ALTER TABLE digests ADD COLUMN topic_id INTEGER REFERENCES topics(id);
ALTER TABLE digests ADD COLUMN run_id   INTEGER NOT NULL DEFAULT 0;
CREATE TABLE IF NOT EXISTS digest_items (
    id           INTEGER PRIMARY KEY,
    digest_id    INTEGER NOT NULL REFERENCES digests(id),
    source_id    INTEGER,                      -- NULL = источник неизвестен (Go 0 → NULL)
    dedup_key    TEXT NOT NULL,
    title        TEXT NOT NULL DEFAULT '',
    url          TEXT NOT NULL DEFAULT '',
    tg_peer      TEXT NOT NULL DEFAULT '',     -- username (без @) или "-100X"
    tg_msg_id    INTEGER NOT NULL DEFAULT 0,
    published_at TEXT NOT NULL DEFAULT '',     -- RFC3339 UTC
    snippet      TEXT NOT NULL DEFAULT '',     -- первые 200 рун Text
    summary      TEXT NOT NULL DEFAULT ''      -- LLM-строка элемента (для L2 в D)
);
CREATE INDEX IF NOT EXISTS digest_items_digest ON digest_items(digest_id);
```

`run_id` — дополнение к исходному плану C (решение планировщика: нужен для
восстановления оглавления по `TOC:`); в комментарии к блоку v4 повторить стиль
v3 (ALTER ADD COLUMN с REFERENCES легален — default NULL; FK не enforcement'ится).
Старые строки digests: `topic_id` NULL, `run_id` 0. **`run_id=0` = «не прогон»**:
`ListRunDigests(0)` возвращает пустой результат, viewer для такого дайджеста
кнопку «◀ К оглавлению» не рисует.

**`internal/storage/digests.go` (новый файл) — структуры и точные сигнатуры:**

```go
type Digest struct {
    ID        int64
    RunID     int64
    TopicID   int64  // читается COALESCE(topic_id, 1)
    CreatedAt string // RFC3339 UTC
    ItemCount int
    Body      string // готовый HTML L1-шапки темы (RenderTopicBody), сохранён при вставке
    Delivered bool
}

type DigestItem struct {
    ID          int64  // 0 при записи; заполняется при чтении
    SourceID    int64  // 0 ↔ NULL (писать nil-аргументом)
    DedupKey    string
    Title       string
    URL         string
    TgPeer      string
    TgMsgID     int64
    PublishedAt string
    Snippet     string
    Summary     string
}

// Пополнение интерфейса Store (store.go):
InsertDigest(ctx context.Context, runID, topicID int64, body string, itemCount int) (digestID int64, err error)
MarkDelivered(ctx context.Context, digestID int64, items []DigestItem) error
GetDigest(ctx context.Context, digestID int64) (d Digest, ok bool, err error) // ok=false при sql.ErrNoRows — НЕ ошибка (решение планировщика)
ListDigestItems(ctx context.Context, digestID int64) ([]DigestItem, error)    // ORDER BY id ASC
ListRunDigests(ctx context.Context, runID int64) ([]Digest, error)            // ORDER BY topic_id ASC; runID=0 → пусто

// Конструктор строки из feed.Item (storage уже импортирует feed):
func NewDigestItem(it feed.Item, summary string) DigestItem
```

- `InsertDigest`: `INSERT INTO digests(created_at, item_count, body, delivered,
  topic_id, run_id) VALUES(?,?,?,0,?,?)`, created_at — RFC3339 UTC now;
  возвращает `LastInsertId`.
- `MarkDelivered` — **одна транзакция**: `UPDATE digests SET delivered=1 WHERE
  id=?`; prepared-insert строк `digest_items`; prepared
  `INSERT OR IGNORE INTO seen_items(dedup_key, source_id, seen_at) VALUES(?,?,?)`
  с source_id = `DigestItem.SourceID` (0 → NULL — решение планировщика:
  информативнее текущего NULL в MarkSeen). Любая ошибка → rollback, ошибка
  наружу. `rows.Close()`, ctx везде (§6).
- `NewDigestItem`: `DedupKey = it.DedupKey()`; `TgPeer`/`TgMsgID` — только для
  `Kind` с префиксом `tg_`: `TgPeer = strings.TrimPrefix(it.Source, "@")`
  (у tg_mtproto `Item.Source` уже в форме `-100X` — A1),
  `TgMsgID = strconv.ParseInt(it.ID, 10, 64)` (ошибка парсинга → 0);
  `PublishedAt = it.Published.UTC().Format(time.RFC3339)`; `Snippet` — первые
  200 рун `it.Text` (локальный helper `truncateRunes` в storage — llm-версия
  неэкспортирована, дублирование 5 строк приемлемо); `Summary` — из аргумента.
- **`SaveDigest` удалить** из интерфейса `Store` и из `SQLiteStore`; вызовы:
  `pipeline.go` (переписывается) и `store_test.go` (заменить тестами новых
  методов). `MarkSeen` в интерфейсе остаётся (используется тестами; новый Run
  его не вызывает — отметка seen живёт внутри MarkDelivered).

**Перепись pipeline (`internal/digest/pipeline.go`) — точные типы:**

```go
// internal/feed/source.go: Item получает поля SourceID, TopicID int64
// (0 = «не проставлен»; DedupKey() НЕ меняется; поля аддитивны — остальные
// пакеты не ломаются).

type TrackedSource struct {
    Src      feed.Source
    SourceID int64 // 0 — строки БД нет (например static не upsert'нулся)
    TopicID  int64 // 0 → «Без темы» (1)
}

type taggedSource struct { // не экспортируется, pipeline.go (§4.2)
    inner             feed.Source
    sourceID, topicID int64
}
// Name() — делегирование inner. Collect() — вызывает inner, КОПИРУЕТ срез и
// stamp'ит SourceID/TopicID на копии (исходные Item источника не мутируются:
// статические источники переиспользуются между прогонами), ошибку inner
// возвращает как есть вместе с частичными items. Уточнение по факту
// реализации (2026-10-20): сам feed.Collect последователен и items упавшего
// источника ВЫБРАСЫВАЕТ (append ошибки + continue) — частичные элементы
// taggedSource в продуктовом пути не потребляются; потери нет (элементы не
// seen, следующий прогон подберёт).

type MessagePoster interface { // удовлетворяется *telegram.Bot
    DeliverWithButtons(ctx context.Context, chatID, text string,
        buttons [][]telegram.InlineButton) error
}

type Pipeline struct {
    SourceProvider func(ctx context.Context) ([]TrackedSource, error) // был []feed.Source
    Store          storage.Store
    TopicSummary   llm.TopicSummarizer // замена Summarize llm.Summarizer
    Poster         MessagePoster       // замена Deliver telegram.Deliverer
    ChatID         string
    Log            *slog.Logger
    Now            func() time.Time
}
```

`Run` — ровно шаги 1–9 «Модели доставки» выше; ошибки шагов 1–2 и 6–7 фатальны,
шаг 5 — skip темы, шаг 8 — log-only. `feed.Collect` не трогать: Run сам
оборачивает каждый `TrackedSource` в `taggedSource` и собирает `[]feed.Source`.
Плоский `llm.Summarizer` pipeline больше не нужен (fallback живёт внутри
`llm.Client.SummarizeTopic`); интерфейс `telegram.Deliverer` сохраняется
(`Bot.Deliver`/`reply` в telegram), но pipeline им больше не пользуется.
Сопоставление summary: `map[int]string` из `ts.Items` по Index; элемент без
summary → `DigestItem.Summary = ""` (L2 в D деградирует на title — §4.10).

**LLM — `internal/llm/topicdigest.go` (новый файл), §4.10:**

```go
type ItemSummary struct { Index int; Summary string } // Index — 1-based позиция в items
type TopicSummary struct {
    Headline string        // plain text; экранируется слоем рендера, не здесь
    Items    []ItemSummary
}
type TopicSummarizer interface {
    SummarizeTopic(ctx context.Context, topicName string, items []feed.Item) (TopicSummary, error)
}

func buildTopicDigestPrompt(topicName string, items []feed.Item) []message // детерминирован
func parseTopicSummary(content string, n int) (TopicSummary, error)        // n = len(items)
```

- System-prompt: требуем **строго JSON**
  `{"headline":"…","items":[{"index":1,"summary":"…"}]}` — по одному summary на
  элемент (index — 1-based порядковый номер из списка), headline ≤2–3
  предложения, **plain text без HTML/маркдауна** (§4.8). User-часть: имя темы +
  пронумерованные элементы (Title + Text, обрезанный `truncateRunes` до 200) —
  паттерн `buildTopicPrompt` из topic.go.
- `parseTopicSummary`: `stripCodeFence` (уже есть) → `json.Unmarshal`; headline
  trim + **обрезка до 800 рун** (решение планировщика: защита от body >4096);
  items с Index вне 1..n и дубли Index **отбрасываются** (первый выигрыш);
  пропущенные индексы — не ошибка. Пустой Headline — не ошибка (у рендера есть
  fallback-ветка).
- `Client.SummarizeTopic` (client.go) — паттерн `SuggestTopic` один в один
  (`maxParseAttempts = 2`): транспортная/HTTP-ошибка из `doWithRetry` → сразу
  error (после фатальных 400/401 не перезапрашиваем); непарсящийся ответ →
  Warn-лог + ровно один повтор; **оба непарсящиеся → fallback**:
  `s, err := c.Summarize(ctx, items)`; err → error наружу; успех →
  `TopicSummary{Headline: s}` (Items пуст — деградация, не падение, §4.10).
  API-ключ никогда не логируется.
- `Offline.SummarizeTopic` (offline.go) — детерминированно (без time/random/
  map-итерации): Headline = до 3 первых элементов списка `- <Title>` (через
  "\n", в порядке входа); Items = на каждый элемент `{i+1, extractLead(it.Text)}`,
  при пустом Text → `it.Title`.
- **Техдолг B L4 закрывается здесь же:** `Client.Summarize` переводится с
  инлайн-декода ответа на существующий `extractContent(resp)` (единый путь
  парсинга; поведение не меняется, client-тесты проходят как раньше).

**Рендеры — `internal/digest/render.go` (новый файл; старый `Render` из
pipeline.go удаляется вместе с render_test.go):**

```go
type TOCEntry struct {
    DigestID  int64
    TopicName string
    ItemCount int
}

// Сохраняется в digests.body при прогоне (готовый HTML под ParseMode):
func RenderTopicBody(topicName, headline string, itemCount int, now time.Time) string
// Формат (golden):
//   📋 <EscapeHTML(topicName)> — <now.Format("02 Jan 2006")>
//   (пустая строка)
//   <EscapeHTML(headline)>       ← при пустом Headline: строки "• <EscapeHTML(title)>"
//   (пустая строка)                 первых 10 элементов (fallback-ветка, решение
//   — N материалов                    планировщика; склонения нет — стиль старого Render)

// L1-view темы (клик T: в C, восстановление L1: в D — функция общая):
func RenderTopicL1(body string, items []storage.DigestItem) string
// body берётся КАК ЕСТЬ (уже готовый HTML — не экранировать повторно!) +
// "\n\n" + по элементу "• <EscapeHTML(truncateRunes(Title, 120))>\n"
// (truncateRunes — локальный helper digest). Items пуст → только body
// (crash-кейс delivered=0). Guard: итог > 4096 рун → items-блок режется
// с хвоста по границе рун с пометкой "…и ещё K"; если всё ещё больше —
// режется body ("…"). Пагинации L1 нет (триггер пересмотра — регулярно
// режущиеся темы > ~50 элементов).

// Оглавление (golden):
func RenderTOC(entries []TOCEntry, date string) string
//   📋 Дайджест — <date>      ← date="" → строка без "— <date>"
//   (пустая строка)
//   Тем: N
//   (пустая строка)
//   • <b>EscapeHTML(TopicName)</b> — <ItemCount> материалов   ← по теме, порядок entries
// date при прогоне = now.Format("02 Jan 2006"); при восстановлении = CreatedAt
// первой строки ListRunDigests, распарсенный RFC3339 и отформатированный так же
// (ошибка парсинга → date="" — решение планировщика).

// Кнопки оглавления — по одной в ряд:
func TOCButtons(entries []TOCEntry) [][]telegram.InlineButton
// {Text: "Открыть: " + telegram.TruncateButtonLabelTo(TopicName, topicOpenBudget),
//  Data: "T:" + strconv.FormatInt(DigestID, 10)}
// topicOpenBudget = 64 − len("Открыть: ") − len("…") — БАЙТОВЫЙ бюджет
// (урок A2 H1: лимит Bot API на текст кнопки — 64 байта, не руны).
```

Решение планировщика: строка оглавления — **имя темы + count**, без headline
(имя стабильно и коротко, headline — внутри L1; «headline или имя» из §4.11a
разрешено в пользу имени).

**Telegram — `bot.go`, `dialogs.go`, `topic_view.go` (новый):**

```go
// bot.go:
type InlineButton struct{ Text, Data string }

type DigestView struct {
    Text    string           // готовый HTML (экранирует вызывающий, telegram повторно не экранирует)
    Buttons [][]InlineButton
}
type DigestViewRequest struct {
    Kind string // ViewKindTopic = "topic" | ViewKindTOC = "toc"
    ID   int64  // topic → digestID; toc → runID
}
// ok=false → «не найдено» (не ошибка); err → «не удалось открыть»:
type DigestViewer func(ctx context.Context, req DigestViewRequest) (view DigestView, ok bool, err error)
func WithDigestViewer(fn DigestViewer) Option // поле digestViewer в Bot

func (b *Bot) DeliverWithButtons(ctx context.Context, chatID, text string,
    buttons [][]InlineButton) error
// chatID-семантика Deliver ("" → owner-chat); test-seam — СУЩЕСТВУЮЩИЙ sendFn
// (в его сигнатуре уже есть kb); sendFn==nil && api==nil → error (НЕ тихий
// no-op: Run обязан узнать о сбое доставки); ParseMode HTML;
// InlineButton → models.InlineKeyboardButton{Text, CallbackData};
// len(buttons)==0 → без ReplyMarkup.
```

- `dialogs.go`: в `handleCallbackQuery` добавить две CutPrefix-ветки **после**
  ts:/tl:/tn: и **до** fallback `callbackHandler`: сначала
  `cbPrefixTOC = "TOC:"`, затем `cbPrefixTopicOpen = "T:"`. Существующий
  `truncateButtonLabelTo` переименовать в экспортируемый
  **`TruncateButtonLabelTo(s string, budget int) string`** (поведение то же:
  байтовый бюджет, граница рун, суффикс "…"); `truncateButtonLabel` и
  `buttonLabelBudget` остаются обёртками над ним.
- `topic_view.go`: `handleTopicOpen(ctx, q, arg)` и `handleTOC(ctx, q, arg)` —
  механика A2/B один в один: **сначала `answerCallback(q.ID, "")`** (ack на
  каждый callback); `callbackMessage(q)` — inaccessible (nil Message) →
  **ack-only, return**; `strconv.ParseInt(arg, 10, 64)` — ошибка → edit
  `digestViewBadIDText`; `b.digestViewer == nil` → edit
  `digestViewUnavailableText`; вызов viewer: err → log.Error + edit
  `digestViewErrorText`; !ok → edit `digestViewNotFoundText`; ok →
  `editCallbackMessage(msg, view.Text, kb)` (kb из view.Buttons; пуст → nil).
  Owner-чек уже есть в общем контуре `handleCallbackQuery` — не дублировать.
  Константы текстов: `digestViewUnavailableText = "Просмотр дайджеста
  недоступен."`, `digestViewBadIDText = "❌ Некорректный запрос."`,
  `digestViewNotFoundText = "ℹ️ Дайджест не найден или устарел."`,
  `digestViewErrorText = "❌ Не удалось открыть."`.
  `WithCallbackHandler` не трогать и в app не проводить (это работа D).

**App — `app.go`, `dynamic_sources.go`, `digest_nav.go` (новый):**

- `sourceProvider` возвращает `[]digest.TrackedSource`; **одна выборка
  `ListSources` на вызов** (уже так — сохранить):
  - статики: `byRef := map[storage.StaticRef]storage.Source` из dbRows по
    (Kind, Ref); zip параллельных срезов `staticSources[i]` + `staticRefs[i]` →
    `TrackedSource{Src, row.ID, row.TopicID}`; строки нет (upsert не
    сработал/pruning-гонка) → `SourceID=0, TopicID=0` → «Без темы» (решение
    планировщика: прогон не останавливается); `enabled` static-строки
    игнорируется, как и раньше (§4.1 — решает config).
  - динамики: `buildDynamicSources` (dynamic_sources.go) — **сигнатура →
    `[]digest.TrackedSource`**: `SourceID = ds.ID`, `TopicID = ds.TopicID` из
    строки; фильтры (disabled, origin='static') не меняются (инвариант B).
- `pipeline`: поля `SourceProvider` (новая сигнатура), `TopicSummary`
  (переменная `summarizer` типизируется шире: offline → `llm.NewOffline()`,
  иначе `*llm.Client` — оба реализуют `TopicSummarizer`), после `NewBot`:
  `pipeline.Poster = bot` (вместо `pipeline.Deliver = bot`). Wiring
  `WithTopicSuggestion` (итог B: только для `*llm.Client`) не трогать.
- `botOpts += telegram.WithDigestViewer(newDigestViewer(store))` — всегда
  (store есть в любом режиме).
- `internal/app/digest_nav.go`:

```go
func newDigestViewer(store *storage.SQLiteStore) telegram.DigestViewer
```

  - `Kind == ViewKindTopic`: `GetDigest(ID)`; !ok → `(…, false, nil)`;
    `ListDigestItems(ID)` → `Text = digest.RenderTopicL1(d.Body, items)`;
    Buttons — один ряд `{Text: "◀ К оглавлению", Data: "TOC:<d.RunID>"}`
    **только при `d.RunID != 0`** (у легаси-строк оглавления нет — кнопки
    нет, решение планировщика).
  - `Kind == ViewKindTOC`: `ID == 0` → `(…, false, nil)`; `ListRunDigests(ID)`
    (пусто → false); `ListTopics` → map id→name (нет имени → `Тема #<id>`) →
    `[]digest.TOCEntry` → `Text = digest.RenderTOC(entries, date)` (date из
    CreatedAt первой строки) + `Buttons = digest.TOCButtons(entries)`.
  - Ошибки storage → `(DigestView{}, false, err)`; crafted-id не роняют viewer.

**Тексты /start,/help (C — подэтап B обещал добавить строку именно здесь):**
дописать в оба статических текста (router.go) строку
`«Дайджест приходит оглавлением: один список тем, каждая открывается кнопкой»`.
Плейсхолдеры по-прежнему только в квадратных скобках; тексты статические —
экранирование не требуется (§4.8).

**Файлы C (точный список):**

| Файл | Роль |
|---|---|
| `internal/feed/source.go` | Item.SourceID/TopicID (аддитивно) |
| `internal/storage/migrate.go` | миграция v4 |
| `internal/storage/digests.go` (новый) | Digest/DigestItem, InsertDigest, MarkDelivered, GetDigest, ListDigestItems, ListRunDigests, NewDigestItem |
| `internal/storage/store.go` | интерфейс: +5 методов, −SaveDigest (реализация удаляется) |
| `internal/llm/topicdigest.go` (новый) | TopicSummarizer, TopicSummary/ItemSummary, prompt + парсер |
| `internal/llm/client.go` | SummarizeTopic; Summarize через extractContent (техдолг B L4) |
| `internal/llm/offline.go` | Offline.SummarizeTopic |
| `internal/digest/pipeline.go` | TrackedSource, taggedSource, MessagePoster, новый Run; −Render |
| `internal/digest/render.go` (новый) | TOCEntry, RenderTopicBody, RenderTopicL1, RenderTOC, TOCButtons |
| `internal/telegram/bot.go` | InlineButton, DeliverWithButtons, DigestViewer/DigestView/DigestViewRequest, WithDigestViewer |
| `internal/telegram/dialogs.go` | ветки TOC:/T: в handleCallbackQuery; экспорт TruncateButtonLabelTo |
| `internal/telegram/topic_view.go` (новый) | handleTopicOpen/handleTOC + константы |
| `internal/telegram/router.go` | строка /start,/help про дайджест-оглавление |
| `internal/app/app.go` | sourceProvider → TrackedSource; pipeline.TopicSummary/Poster; WithDigestViewer |
| `internal/app/dynamic_sources.go` | buildDynamicSources → []digest.TrackedSource |
| `internal/app/digest_nav.go` (новый) | newDigestViewer(store) |
| тесты + `internal/digest/testdata/` (создать) | кейсы ниже; render_test.go и pipeline_test.go заменяются |

**Не трогать:** scheduler; `internal/mtproto/*`; config; Dockerfile/compose;
`topics_admin.go`, `forward.go`, `source_admin.go`, `links.go`; legacy-опции и
seams A1/A2/B (их «Итоги» — инварианты); файлы раздела D (drilldown.go,
CallbackResult, DeepLink — D).

**Тест-кейсы C (обязательный перечень; table-driven + golden + temp SQLite +
`-race`; распределение по C1/C2 — в «Оценке размера» ниже):**

1. *Миграция v4:* поверх v3 → user_version=4, повторный Open идемпотентен;
   старая строка digests получает topic_id NULL/run_id 0; digest_items и индекс
   существуют.
2. *storage/digests_test.go:* InsertDigest — delivered=0, id последовательны;
   MarkDelivered — одна транзакция: delivered=1, строки digest_items,
   seen_items (последующий FilterUnseen возвращает пусто); SourceID=0 → NULL;
   rollback при ошибке (способ воспроизведения: DROP TABLE digest_items
   внутри живой tx роняет prepared-insert; «пустой dedup_key» НЕ работает —
   '' не нарушает NOT NULL в SQLite, поправка по факту реализации C1);
   GetDigest ok / not-ok (crafted id — без ошибки); ListDigestItems — порядок
   id ASC; ListRunDigests — два прогона не смешиваются, порядок topic_id ASC,
   runID=0 → пусто; restart-survival: Close → Open → GetDigest+ListDigestItems
   на месте.
3. *pipeline_test.go — реальный Run поверх фейков* (Poster записывает вызовы;
   Store — настоящая temp SQLite; TopicSummarizer — записывающий фейк; Now
   фиксирован):
   - группировка: темы 1 и 5 + элементы TopicID=0 → **одно** сообщение
     оглавления, entries в порядке 1,5, нули слиты в «Без темы»; кнопки
     `T:<id1>`, `T:<id5>`; пустая тема (нет fresh-элементов) в оглавление не
     попадает;
   - taggedSource: stamp на копии (исходные Item источника не изменены);
     частичная ошибка источника → items+err оба возвращены;
   - нет fresh → Poster не вызван, Run=nil, строк digests нет;
   - частичный сбой: SummarizeTopic темы 1 → error → в оглавлении только тема
     2; элементы темы 1 НЕ seen (FilterUnseen их видит), темы 2 — seen;
     Run=nil; строки digests темы 1 нет;
   - все темы упали → Run вернул ошибку, оглавления нет, элементы не seen
     (слот не отмечается: runJob ставит setSlot только при успехе —
     существующее поведение scheduler, нового теста не требует);
   - DeliverWithButtons → error → Run error, все строки delivered=0, все
     элементы не seen, MarkDelivered не вызван;
   - сбой MarkDelivered после успешной доставки → Run=nil, оглавление отправлено;
   - `-race`: Collect нескольких taggedSource одним feed.Collect (обход
     последовательный — формулировка уточнена по факту реализации).
4. *llm/topicdigest_test.go (httptest):* валидный JSON; JSON в ``` fence;
   мусор → ровно 2 запроса → fallback Headline=Summarize, Items пуст (сервер
   считает вызовы); мусор + Summarize-мусор → ошибка; index вне диапазона и
   дубли → отброшены; headline >800 рун → обрезан; offline: два вызова
   байт-в-байт; Headline ≤3 строк `- Title`; ItemSummary — первое предложение
   Text, пустой Text → Title.
5. *Рендеры (golden, internal/digest/testdata/):* RenderTopicBody — обычная
   тема, «Без темы», пустой Headline (fallback заголовков); RenderTopicL1 —
   обычный, пустой items, overflow >4096 («…и ещё K», затем резка body);
   RenderTOC — 1 тема, 10 тем, date="" и date задан; TOCButtons — data-формат.
   *Байт-лимит кнопок:* кириллическое имя темы 30 рун и emoji-имя →
   `len(button.Text) ≤ 64` БАЙТА (урок A2 H1).
6. *telegram (seams ackFn/editFn/sendFn):* DeliverWithButtons — chatID ""→
   owner, невалидный chatID → error, конвертация кнопок в markup, ошибка seam →
   error наружу, api==nil && sendFn==nil → error; callback T:/TOC: — owner
   (ack→edit порядок на счётчиках), не-owner (ack «Не ваш бот», без edit),
   inaccessible (ack-only), нечисловой arg → badID, viewer nil → unavailable,
   ok=false → notFound, err → errorText, успех → editCallbackMessage c
   view.Text/Buttons; **после «рестарта»** — тот же store на temp SQLite,
   новый Bot: T: рендерит из БД (данные не из памяти).
7. *app:* newDigestViewer — topic (RunID≠0 → TOC-кнопка; RunID=0 → без
   кнопки), toc (date из CreatedAt; битый CreatedAt → date=""), miss → ok=false;
   sourceProvider — ListSources вызван ОДИН раз (счётчик фейка), статики
   получают SourceID/TopicID из строк, отсутствующая строка → нули, динамики —
   из своих строк (buildDynamicSources-тест обновить под TrackedSource).

**Критерий готовности C:** `go build ./... && go vet ./... && gofmt -l .` (пусто)
`&& go test -race ./...` зелёные; golden обновлены осознанно; кейсы 1–7
покрыты; ручной сценарий минимум один раз до PR (реальный токен): `/digest` →
оглавление → `T:` открывает тему → `TOC:` возвращает → **рестарт бота** →
`T:`/`TOC:` работают из БД; offline-режим (без LLM_API_KEY) — детерминированный
headline без падений. Docs: TECHNICAL_PLAN.md §7 (schema: digests.topic_id/
run_id, digest_items) и §9 (статус C) — только своя часть.

**Оценка reviewable-размера и разбиение (§8):** оценка ~1800–2200 строк с
тестами — выше порога ~1500, поэтому **обязательное разбиение на два PR**
(решение планировщика), порядок сдачи **C1 → C2**:

- **C1 — данные и рендеры, ноль изменения поведения:** feed.Item-поля;
  миграция v4; storage/digests.go + интерфейс (**SaveDigest пока жив** — старый
  pipeline компилируется и работает); llm/topicdigest.go + client/offline +
  техдолг L4; digest/render.go (Render пока жив); типы TrackedSource/
  taggedSource (не подключены). Тесты: кейсы 1, 2, 4, 5 (golden рендеров).
  Сборка зелёная, поведение бота не меняется — ревьюится SQL/транзакции/LLM.
- **C2 — доставка и навигация:** перепись Run (SourceProvider→TrackedSource,
  Poster, TopicSummary); удаление SaveDigest и старого Render/render_test.go;
  telegram (InlineButton, DeliverWithButtons, TOC:/T:-ветки,
  TruncateButtonLabelTo, topic_view.go); app (sourceProvider,
  dynamic_sources, digest_nav, wiring); тексты /start,/help. Тесты: кейсы 3,
  6, 7 + restart-survival из кейса 2 на уровне pipeline. Фокус ревью —
  семантика частичного сбоя (шаги 5–8 модели доставки) и байтовые лимиты
  кнопок.

**Итог C (реализован 2026-10-20, сдан двумя PR: C1-данные → C2-доставка):**

- Прогон: build/vet/gofmt чистые, `go test -race` 8/8 пакетов ok (C2: 236
  RUN/PASS в digest/telegram/app, 0 skip). Ревью: C1 — 0 high/2 medium/4 low
  (M1 panic-полоса RenderTopicL1 и M2 cap body закрыты фиксами + защиты
  L1/L2/L4; L3/L5/L6 приняты), C2 — 0 high/0 medium/5 low (LOW-1 доки
  исправлены; остальные приняты, см. техдолг). 9-шаговая модель доставки
  подтверждена ревью по file:line; контракты C1/D (RenderTopicL1, Store) целы.
- **Подтверждённые отклонения от плана** (не переоткрывать):
  - RenderTopicBody — variadic `fallbackTitles ...string` (план внутренне
    противоречив: сигнатура с itemCount, но формат требует тайтлы); Run
    передаёт ≤10 заголовков — fallback-ветка живая, покрыта тестом.
  - Общий `handleDigestView` для T:/TOC: (вместо двух веток с дублем).
  - `newSourceProvider` + sourceLister-интерфейс в app — ради счётчика
    ListSources (тест-кейс 7: одна выборка на прогон).
  - telegram-restart-тест — viewer над storage без digest-рендеров (цикл
    импорта); полный рендерный путь покрыт digest/app-тестами.
  - Rollback-тест MarkDelivered — через DROP TABLE digest_items в живой tx
    ('' не нарушает NOT NULL в SQLite; текст кейса 2 поправлен).
  - InsertDigest нормализует topicID 0→NULL (защита контракта сверх плана:
    COALESCE(topic_id,1) срабатывает даже если вызывающий забыл map 0→1).
- **Техдолг C** (триггеры пересмотра):
  1. LOW-2: конкурентные Run (/digest вручную + scheduler) без mutex — дубль
     оглавления; pre-existing, single-user; триггер: дубли в живом
     использовании → singleflight/mutex вокруг Run.
  2. LOW-3: edit с nil-клавиатурой оставляет старые кнопки под текстом
     «не найдено/ошибка» — консонистентно с A2/B; триггер: UX-жалоба.
  3. LOW-4: date восстановленного оглавления — из CreatedAt (UTC) без
     конвертации в локальную TZ (план-предписано); около полуночи возможна
     вчерашняя дата; триггер: сообщение пользователя.
  4. LOW-5: Run паникует при nil-зависимостях (wiring в app корректен,
     контракт «Must be non-nil» в комментарии); триггер: новый вызывающий.
  5. Пагинации оглавления нет (кнопки в один ряд); триггер: >30 тем →
     префикс TOCP:. L1 >4096 рун режется без пагинации; триггер: регулярно
     режущиеся темы >~50 элементов.
- **Ручной сценарий с реальным токеном** (критерий готовности C) в среде
  агентов невыполним (нет токена; второй long-polling-инстанс конфликтует с
  прод-ботом на VPS — 409). ПЕРЕНЕСЁН на деплой: /digest → оглавление →
  T: открывает тему → TOC: возвращает → рестарт бота → T:/TOC: из БД;
  offline-режим — детерминированное оглавление без падений.

---

### Подэтап D — drill-down L1→L2→L3

**Файлы:** `digest/drilldown.go` (новый), `telegram/router.go` (полноценная
ветка CallbackQuery — каркас из A2 расширяется до диспетчера),
`telegram/bot.go` (`EditMessage`, `AnswerCallback`), `storage/digests.go`
(выборки), `digest/deeplink.go` или `telegram/links.go` (построение deep link).

**Новые Store-выборки:**

```go
GetDigest(ctx context.Context, digestID int64) (Digest, error)
    // Digest{ID, CreatedAt, ItemCount, Body, Delivered, TopicID}
ListDigestItems(ctx context.Context, digestID int64) ([]DigestItem, error)         // для L2 (группировка по source_id в рендере)
ListDigestItemsBySource(ctx context.Context, digestID, sourceID int64) ([]DigestItem, error) // для L3
```

**Обработчик** (в digest, wiring в app через `telegram.WithCallbackHandler`):

```go
func NewDrillDown(store storage.Store, sources func(ctx) ([]storage.Source, error))
    func(ctx context.Context, data string) (telegram.CallbackResult, error)
```

```go
// telegram.Bot (bot.go):
type InlineButton struct{ Text, Data string }
type CallbackResult struct {
    AckText string      // AnswerCallbackQuery
    Edit    *EditMessage // nil → ничего не редактируем
}
type EditMessage struct { Text string; Buttons [][]InlineButton }
func WithCallbackHandler(fn func(ctx context.Context, data string) (CallbackResult, error)) Option
```

Router: `u.CallbackQuery != nil` → авторизация `From.ID` → handler(data) →
`AnswerCallbackQuery` → при `Edit != nil` и доступном
`u.CallbackQuery.Message` (`*models.Message`) → `EditMessageText` с
`ReplyMarkup` из кнопок. Ошибка handler'а → ack «Не получилось открыть» + лог.

**Рендер L2** (`L2:<digestID>`): заголовок «📰 <тема> — по источникам», секции:
`<b><EscapeHTML(sourceName)></b>` (имя — из sources-снапшота; source_id без
строки → «источник #N»), под секцией bullet'ы `summary` элементов (пустой
summary → `title`). Кнопки: по одной «<sourceName> → сообщения» на секцию
(`L3:<did>:<srcID>`, текст ≤60 рун, обрезаем) + «◀ Назад» (`L1:<digestID>`).
Лимит: >8 секций → кнопки в две колонки/страницы не делаем в этом этапе;
секций >15 — только «Назад» + текст «слишком много источников, см. L3 общим
списком» (кнопка `L3:<did>:0` = все элементы). Простое решение, триггер
пересмотра в §7.

**Рендер L3** (`L3:<digestID>:<sourceID|0>`): список элементов:
`• <a href="LINK">title</a> — <EscapeHTML(snippet≤120 рун)>`, где LINK:
`url != ""` → url; иначе tg_peer/tg_msg_id → deep link: username → 
`https://t.me/<user>/<msgID>`, `-100X` → `https://t.me/c/<X>/<msgID>`
(`telegram.DeepLink(tgPeer string, msgID int64) string` — рядом с ParseTmeLink,
обратная конверсия через `mtproto.ToMTProtoID`-логику, но без импорта mtproto:
локальный strip `-100`). Кнопка «◀ Назад» → `L2:<digestID>`. Сообщение >4096
символов — обрезаем список с пометкой «…и ещё K» (пагинация L3 — техдолг, §7).

**L1-восстановление** (`L1:<digestID>`): тело из `digests.body` + кнопка
«Подробнее» (`L2:<digestID>`).

**Тест-кейсы D:** парсер callback_data (все префиксы, мусор, переполнение,
не-числа); L2/L3 golden-рендеры (с секциями, пустым summary, длинным snippet,
обрезкой 4096); DeepLink: публичный/приватный/битый peer; drilldown на
несуществующем digestID → ack-ошибка, не паника; router: callback от
не-owner → игнор + ack; InaccessibleMessage → только ack; edit-порядок
(сначала ack, потом edit — проверяем на фейковом api-слое, если он есть в
router_test; иначе — на уровне handler-возврата).

---

## 6. Сквозные требования ко всем подэтапам

- Все запросы с ctx; транзакции для многошаговых записей (MarkDelivered,
  AddSourceIfNotExists). `rows.Close()` везде.
- Логи `log/slog`; в логах не появляется: токен, LLM-ключ, access_hash, тело
  сессии. access_hash — чувствительные метаданные: не логировать значение.
- Новые пользовательские строки — без угловых скобок в плейсхолдерах (квадратные),
  весь динамический текст через `EscapeHTML`.
- Тесты — table-driven + golden (`-update`), фейки вместо сети, temp SQLite
  (`t.TempDir()`), `-race`. Интеграционный ярус SKILL.md §7 сохраняется:
  pipeline-тесты гоняют реальный Run поверх фейков.
- Каждый подэтап: `go build ./... && go vet ./... && gofmt -l . && go test -race ./...`
  зелёные **до** коммита; conventional-commit с русским subject.

## 7. Риски и триггеры пересмотра

| Риск | Митигация | Триггер пересмотра |
|---|---|---|
| Имена типов `models.MessageOrigin*` отличаются от ожидаемых | v1.22.0 проверен: ForwardOrigin есть; при реализации открыть `models/reply.go` | тип отсутствует → bump go-telegram/bot отдельным коммитом |
| LLM не держит JSON-формат | повтор + fallback на Summarize/заголовки | >30% фолбэков в логах за неделю → сменить формат на «строка на элемент» без JSON |
| FloodWait при многих приватных источниках | раз в сутки/источник, backoff+jitter, кеш пиров | прогон >10 мин или FLOOD_WAIT в каждодневных логах → батчинг getDialogs-чтения, лимит источников на прогон |
| access_hash протух (CHANNEL_INVALID) | один диалог-рефреш на ошибку | повторные invalid после рефреша → авто-disable источника + уведомление owner |
| Тем-сообщений слишком много (>10/день) | **решено 2026-10-20**: L2-оглавление одним сообщением (§4.11a), пользователь планирует 10+ тем | триггер активирован заранее — митигация перенесена в перепланинг подэтапа C |
| L3 не влезает в 4096 | обрезка «…и ещё K» | регулярно режется >30% сообщений → пагинация L3 (callback `L3p:<did>:<src>:<page>`) |
| Пустые suggested-темы копятся | принимаем | >20 пустых тем → prune-запрос в /topics |
| digest_items растёт | один пользователь, объёмы малы | БД >200 МБ → retention-политика (хранить N дней) |
| UNIQUE(kind,ref) падает на прод-БД с дублями | миграция падает явно, чистка руками | случилось → скрипт-дедуп в docs/handoff |
| Static-строки путаются с dynamic в /sources | колонка origin, фильтр в buildDynamicSources | /sources стал нечитаемым → группировка вывода по темам |

## 8. Порядок сдачи и оценка reviewable-размера

1. **PR A1** — самый крупный (~15 файлов); если в процессе diff превышает ~1200
   строк, расщепить по границе «buffer+links+forward» / «mtproto peers+history»,
   миграцию v2 несёт первый.
2. **PR A2** — dialogs (небольшой, переиспользует A1).
3. **PR B** — topics schema+commands+suggestion.
4. **PR C1 + PR C2** — per-topic digest: разбиение по разделу подэтапа C
   «Оценка reviewable-размера и разбиение» (C1 — схема/storage/llm/рендеры без
   изменения поведения; C2 — перепись pipeline + доставка оглавлением +
   навигация T:/TOC:; ревью особенно внимательно к семантике частичного сбоя
   §4.9/§4.11a).
5. **PR D** — drill-down (только чтение + edit, низкий риск).

Между PR — финальный зелёный прогон в основной сессии и пункт 7 пайплайна
(актуализация docs/TECHNICAL_PLAN.md §7 схема + §9 статусы этапов — в каждом PR
трогаем только свою часть).
