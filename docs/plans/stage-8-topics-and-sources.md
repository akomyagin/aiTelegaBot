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
3. **Дайджест — отдельное сообщение на каждую тему** (один прогон = N сообщений).
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
- Дневной прогон шлёт по одному сообщению на тему (пустые темы пропускаются);
  сбой доставки одной темы не теряет элементы других и не блокирует слот.
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
L1-кнопка «Подробнее» на сообщении темы:  "L2:<digestID>"
L2-кнопка источника:                      "L3:<digestID>:<sourceID>"
L2-кнопка «Назад»:                        "L1:<digestID>"
L3-кнопка «Назад»:                        "L2:<digestID>"
Подтверждение темы при добавлении:        "ts:<sourceID>:<topicID>"
Выбрать другую тему:                      "tl:<sourceID>"
Остаться «Без темы»:                      "tn:<sourceID>"
Кнопка «добавить» в /dialogs:             "ad:<botAPIID>"   (int64, ≤19 цифр)
Пагинация /dialogs:                       "dp:<offset>"
```

Худший случай `L3:` + 19 + `:` + 19 = 42 байта < 64. Все id — десятичные, без
имён/строк. Парсер — `strings.CutPrefix` + `strings.Cut`, не regex.

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

---

### Подэтап C — дайджест по темам

> **Внимание (решение 2026-10-20):** модель доставки — не «N сообщений по
> темам», а **одно сообщение-оглавление** (§4.11a). Раздел ниже описывает
> исходный замысел и перед стартом C подлежит переработке планировщиком
> (Agent `model: "opus"`): запись digests на тему сохраняется, доставка и
> RenderTopic заменяются рендером оглавления + кнопками `T:<digestID>`.

**Файлы:** `digest/pipeline.go` (переписывается Run по §4.9; `TrackedSource`,
`taggedSource`), `digest/render.go`-часть (Render → `RenderTopic`),
`llm/topicdigest.go` (новый), `llm/client.go`, `llm/offline.go`,
`storage/migrate.go` (v4), `storage/digests.go` (новый: InsertDigest,
MarkDelivered+items, выборки для D), `store.go` (интерфейс: SaveDigest уходит —
заменяется на InsertDigest/MarkDelivered; `SaveDigest` удалить, обновив вызовы),
`telegram/bot.go` (`DeliverWithButtons`, тип `InlineButton`),
`internal/app/app.go` (SourceProvider → TrackedSource; wiring TopicSummarizer),
golden-файлы `internal/digest/testdata/`.

**Миграция v4:**

```sql
ALTER TABLE digests ADD COLUMN topic_id INTEGER REFERENCES topics(id);
CREATE TABLE IF NOT EXISTS digest_items (
    id           INTEGER PRIMARY KEY,
    digest_id    INTEGER NOT NULL REFERENCES digests(id),
    source_id    INTEGER,
    dedup_key    TEXT NOT NULL,
    title        TEXT NOT NULL DEFAULT '',
    url          TEXT NOT NULL DEFAULT '',
    tg_peer      TEXT NOT NULL DEFAULT '', -- username или -100X
    tg_msg_id    INTEGER NOT NULL DEFAULT 0,
    published_at TEXT NOT NULL DEFAULT '',
    snippet      TEXT NOT NULL DEFAULT '', -- первые ~200 рун Text
    summary      TEXT NOT NULL DEFAULT ''  -- LLM-строка элемента (L2)
);
CREATE INDEX IF NOT EXISTS digest_items_digest ON digest_items(digest_id);
```

**Новые Store-методы:**

```go
InsertDigest(ctx context.Context, topicID int64, body string, itemCount int) (digestID int64, err error) // delivered=0
MarkDelivered(ctx context.Context, digestID int64, items []DigestItem) error // tx: delivered=1 + insert digest_items
```

`storage.DigestItem` — зеркало строки digest_items (без id). Заполнение из
`feed.Item`: tg_peer/tg_msg_id — для Kind `tg_*` (Source как username/`-100X`,
ID → int), snippet — `truncateRunes(Text, 200)`, url — Item.URL.

**Новый Run (pipeline.go):**

```go
type TrackedSource struct { Src feed.Source; SourceID, TopicID int64 }
type Pipeline struct {
    SourceProvider func(ctx context.Context) ([]TrackedSource, error)
    Store storage.Store
    Summarize llm.TopicSummarizer   // замена llm.Summarizer
    Poster  MessagePoster           // ниже
    ChatID string; Log; Now
}
type MessagePoster interface {     // удовлетворяется *telegram.Bot
    Deliver(ctx context.Context, chatID, text string) error
    DeliverWithButtons(ctx context.Context, chatID, text string,
        buttons [][]telegram.InlineButton) error
}
```

Шаги: collect (через taggedSource-обёртки) → FilterUnseen → group by TopicID
(0→1) → для каждой темы (порядок: topics.id asc, имя берётся запросом/кешем
ListTopics): SummarizeTopic → RenderTopic → InsertDigest → DeliverWithButtons
(кнопка «Подробнее» = `L2:<digestID>`) → MarkSeen(элементы темы) →
MarkDelivered(digestID, items). Ошибка Summarize одной темы — как ошибка
доставки темы (лог, skip, остальные живут); ошибка шага 1–3 (до цикла) — фатальна.

**RenderTopic** (детерминирован, golden):

```
📋 <EscapeHTML(topicName)> — <date>

<EscapeHTML(Headline)>

— N материалов
```

Если `Headline` пуст (fallback без Items) — рендер из заголовков элементов.
L2/L3-рендеры — в подэтапе D (тот же пакет).

**Тексты /start,/help (C):** «Дайджест приходит отдельным сообщением на каждую
тему; кнопка «Подробнее» раскрывает детали».

**Тест-кейсы C:** миграция v4; группировка по темам (фейковый Poster записывает
порядок и кнопки): 2 темы → 2 сообщения, пустая тема → 0 сообщений, элементы с
TopicID=0 → «Без темы»; сбой доставки темы 1 → элементы темы 1 не seen, тема 2
доставлена и seen, `Run` вернул nil; InsertDigest/MarkDelivered roundtrip +
restart-survival (закрыть/открыть БД, digest_items на месте); golden RenderTopic
(обычная тема, «Без темы», пустой Headline-fallback); taggedSource stamp'ит
SourceID/TopicID и не мутирует исходные Item; llm client SummarizeTopic —
httptest JSON ok/fence/мусор-fallback; offline SummarizeTopic детерминирован
(два вызова — байт-в-байт); `-race` на параллельный Collect.

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
4. **PR C** — per-topic digest (переписывает pipeline — ревью особенно внимательно
   к семантике частичного сбоя §4.9).
5. **PR D** — drill-down (только чтение + edit, низкий риск).

Между PR — финальный зелёный прогон в основной сессии и пункт 7 пайплайна
(актуализация docs/TECHNICAL_PLAN.md §7 схема + §9 статусы этапов — в каждом PR
трогаем только свою часть).
