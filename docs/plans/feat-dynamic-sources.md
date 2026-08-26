# План: динамическое управление источниками (`feat/dynamic-sources`)

Исполнитель — Opus по этому файлу. Ветка `feat/dynamic-sources` уже создана от
`master`. Коммиты/пуш НЕ делать (это делает основная сессия).

## 1. Цель

Пользователь через Telegram-команды добавляет и удаляет источники дайджеста без
правки `.env` и рестарта бота:

- `/addsource <url|@channel>` — определяет вид источника, СРАЗУ валидирует пробным
  сбором, при успехе сохраняет в БД (`enabled=1`), сообщает число найденных постов.
- `/removesource <id>` — отключает источник по числовому id.
- `/sources` — расширить: показывать id рядом с каждым источником (в т.ч. из БД).

Источники из БД подхватываются на КАЖДОМ прогоне дайджеста (по расписанию и по
`/digest`) без рестарта. Источники из `.env` — статический слой, остаётся как есть.

## 2. Ограничения и что НЕ трогать

- **НЕ трогать** `internal/mtproto/` — вне скоупа. Через `/addsource` MTProto-источники
  не добавляются (kind `tg_mtproto` руками не создаётся).
- **НЕ трогать** статический слой из `.env`: `FEED_URLS`, `TG_PUBLIC_CHANNELS`,
  `TG_MANAGED_CHANNELS`, `HN_LIMIT`, `TG_SOURCE_LIMIT`. Они по-прежнему строятся в
  `app.Run` и всегда участвуют в прогоне.
- **НЕ вводить** callback-query / inline-кнопки. `/removesource` берёт числовой id из
  текста. Это осознанное упрощение (зафиксировано пользователем).
- Схема таблицы `sources` (`internal/storage/migrate.go:14-20`) уже существует
  (migration v1). НЕ добавлять новую миграцию — таблица уже есть, просто начать её
  использовать. Колонки: `id INTEGER PK`, `kind TEXT`, `ref TEXT`, `enabled INTEGER
  DEFAULT 1`, `added_at TEXT`.
- Секреты (bot-токен) не логировать. Ошибки Telegram оборачивать без токена.
- Команды принимать только от владельца (`authorized()` уже фильтрует в `handleUpdate`).

## 3. Инварианты, которые надо сохранить

- `feed.Source` (`internal/feed/source.go:48-51`): `Collect(ctx) ([]Item, error)` +
  `Name() string`. Любой динамический источник — обычный `feed.Source`.
- `feed.Item.DedupKey()` уже разводит web (по URL) и TG (по `tg:<source>:<id>`).
  Динамические источники используют те же конструкторы, что и статические, поэтому
  DedupKey и дедуп работают без изменений. Один и тот же RSS-URL, добавленный и в
  `.env`, и через `/addsource`, даст одинаковые DedupKey → повторно доставлен не будет
  (дедуп по `seen_items` это гасит). Дублирование в `/sources` допустимо.
- `PublicSource.Collect` деградирует в `(nil, nil)` при ошибке скрапинга
  (`public_source.go:49-56`). Для ВАЛИДАЦИИ при `/addsource` этого недостаточно —
  см. §6.2 (валидация должна уметь отличить «канал пуст/недоступен» от успеха).
- Единственная точка склейки — `internal/app/app.go`. Пакеты ядра знают друг о друге
  через интерфейсы.

---

## 4. Storage: CRUD для `sources`

### 4.1 Go-тип строки — новый файл `internal/storage/sources.go`

```go
package storage

// Source is one row of the sources table (dynamic, DB-backed sources added at
// runtime via /addsource). Kind is one of feed.Item.Kind values managed here:
// "rss" | "tg_botapi" | "tg_public". "arxiv"/"hn"/"tg_mtproto" are NOT created
// via commands.
type Source struct {
	ID      int64
	Kind    string
	Ref     string // RSS URL, or channel username WITHOUT leading '@'
	Enabled bool
	AddedAt string // RFC3339 UTC
}
```

`ref` хранится БЕЗ ведущего `@` для каналов (нормализуется в команде через
`strings.TrimPrefix(x, "@")`, как в `app.go:67`), и как полный URL для rss.

### 4.2 Расширение интерфейса `Store` (`internal/storage/store.go:24-37`)

Добавить в интерфейс `Store` и реализовать на `*SQLiteStore` в `sources.go`:

```go
// AddSource inserts an enabled source and returns its new id.
AddSource(ctx context.Context, kind, ref string) (int64, error)

// ListSources returns all sources ordered by id (enabled and disabled).
ListSources(ctx context.Context) ([]Source, error)

// DisableSource sets enabled=0 for the given id. Returns (false, nil) when no
// row with that id exists (so the command can report "нет источника с id N").
DisableSource(ctx context.Context, id int64) (found bool, err error)
```

Решение: `/removesource` **отключает** (`enabled=0`), НЕ `DELETE`. Причина: строки
`seen_items` могут ссылаться на `source_id`, и мягкое отключение проще и обратимо.
Метод называется `DisableSource`, не `RemoveSource`.

SQL (все через `ExecContext`/`QueryContext` + `context`, стиль как в `store.go`):

```go
func (s *SQLiteStore) AddSource(ctx context.Context, kind, ref string) (int64, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO sources(kind, ref, enabled, added_at) VALUES(?,?,1,?)",
		kind, ref, now)
	if err != nil {
		return 0, fmt.Errorf("insert source: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("last insert id: %w", err)
	}
	return id, nil
}

func (s *SQLiteStore) ListSources(ctx context.Context) ([]Source, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, kind, ref, enabled, added_at FROM sources ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("query sources: %w", err)
	}
	defer rows.Close()
	var out []Source
	for rows.Next() {
		var src Source
		var enabled int
		if err := rows.Scan(&src.ID, &src.Kind, &src.Ref, &enabled, &src.AddedAt); err != nil {
			return nil, fmt.Errorf("scan source: %w", err)
		}
		src.Enabled = enabled != 0
		out = append(out, src)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sources: %w", err)
	}
	return out, nil
}

func (s *SQLiteStore) DisableSource(ctx context.Context, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, "UPDATE sources SET enabled=0 WHERE id=?", id)
	if err != nil {
		return false, fmt.Errorf("disable source %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n > 0, nil
}
```

> Примечание: интерфейс расширяется без «второй реализации», потому что `Store` уже
> интерфейс с единственной реализацией `SQLiteStore` — добавляем методы к
> существующему порту, а не заводим новый интерфейс. Это в рамках конвенций.

---

## 5. Pipeline: динамический подхват источников на каждом Run()

### Решение по развилке (главное)

Заменить статическое поле `Pipeline.Sources []feed.Source` на **провайдер-функцию**:

```go
// Pipeline (internal/digest/pipeline.go)
type Pipeline struct {
	// SourceProvider returns the source set for THIS run. It is called at the
	// start of every Run, so DB-backed sources added at runtime are picked up
	// without a restart. Must be non-nil.
	SourceProvider func(ctx context.Context) ([]feed.Source, error)
	Store     storage.Store
	Summarize llm.Summarizer
	Deliver   telegram.Deliverer
	ChatID    string
	Log       *slog.Logger
	Now       func() time.Time
}
```

В `Run` (сейчас `pipeline.go:47`) заменить:

```go
// было: items, collectErr := feed.Collect(ctx, p.Sources)
sources, err := p.SourceProvider(ctx)
if err != nil {
	return fmt.Errorf("resolve sources: %w", err)
}
items, collectErr := feed.Collect(ctx, sources)
```

Обоснование выбора провайдера-функции (а не, например, `Sources []feed.Source` +
отдельный `Store.ListSources` внутри Pipeline):
- Pipeline не должен сам знать, КАК источники из БД превращаются в `feed.Source`
  (это требует HTTP-клиента, лимитов, конструкторов из `feed`/`telegram`) — эта
  сборка живёт в `app` (единственная точка склейки, конвенция §1 SKILL). Провайдер
  инкапсулирует «статические + динамические» и отдаёт готовый `[]feed.Source`.
- Один вызов на прогон = свежий снимок БД на каждый Run без рестарта. Требование 3
  выполнено буквально.

### 5.1 Правки в `internal/app/app.go`

1. Вынести построение статических источников (`app.go:44-109`, весь блок RSS + HN +
   managed + public + MTProto) в функцию, чтобы её можно было звать на каждом прогоне.
   Разделить на две части:
   - **статические** (RSS/HN/managed/public/MTProto из cfg) — строятся ОДИН раз при
     старте (в них есть stateful: `channelBuf`, `mtClient`), сохраняются в срез
     `staticSources`.
   - **динамические** (из БД) — строятся на каждом прогоне из `store.ListSources`.

2. Ввести билдер динамических источников — новый файл
   `internal/app/dynamic_sources.go`:

```go
package app

import (
	"context"
	"net/http"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
	"github.com/akomyagin/aiTelegaBot/internal/storage"
	"github.com/akomyagin/aiTelegaBot/internal/telegram"
)

// buildDynamicSources maps enabled DB rows to feed.Source implementations,
// reusing the same constructors as the static config layer. Disabled rows are
// skipped. Unknown kinds are skipped (logged), never fatal.
func buildDynamicSources(
	dbSources []storage.Source,
	hc *http.Client,
	tgLimit int,
	channelBuf *telegram.ChannelBuffer, // may be nil
) []feed.Source {
	var out []feed.Source
	for _, ds := range dbSources {
		if !ds.Enabled {
			continue
		}
		switch ds.Kind {
		case "rss":
			out = append(out, feed.NewRSSSource(ds.Ref, ds.Ref, "rss", hc))
		case "tg_public":
			out = append(out, telegram.NewPublicSource("@"+ds.Ref, ds.Ref, hc, tgLimit))
		case "tg_botapi":
			// Managed channel requires the shared buffer wired at startup. If the
			// bot was started without any managed channel, channelBuf is nil — in
			// that case there is no buffer to drain, so skip (log a warning).
			if channelBuf != nil {
				out = append(out, telegram.NewManagedSource("@"+ds.Ref, channelBuf))
			}
		}
	}
	return out
}
```

> **Тонкость tg_botapi + channelBuf.** `ManagedSource` тянет посты из общего
> `ChannelBuffer`, который наполняет `handleUpdate` для КАЖДОГО channel-post
> (`router.go:39-45`) — независимо от того, перечислен ли канал в конфиге. Условие
> буферизации — `b.channelBuf != nil`. Значит: чтобы `/addsource @managed` работал,
> `channelBuf` должен существовать всегда, даже если `TG_MANAGED_CHANNELS` пуст.
> **Правка в `app.go`:** создавать `channelBuf = &telegram.ChannelBuffer{}`
> безусловно (сейчас — только если `len(cfg.TGManagedChannels) > 0`, `app.go:64`) и
> всегда передавать `telegram.WithChannelBuffer(channelBuf)`. Тогда посты
> добавленного managed-канала попадут в буфер и подхватятся `ManagedSource` на
> следующем прогоне. Это единственное изменение вокруг managed-каналов.

3. Собрать `SourceProvider` и передать в Pipeline (заменить `app.go:111-116`):

```go
channelBuf := &telegram.ChannelBuffer{} // всегда, безусловно

// ... staticSources построены выше (RSS/HN/public/MTProto/managed из cfg) ...

sourceProvider := func(ctx context.Context) ([]feed.Source, error) {
	dbRows, err := store.ListSources(ctx)
	if err != nil {
		return nil, err
	}
	dyn := buildDynamicSources(dbRows, hc, tgLimit, channelBuf)
	// static first, then dynamic; order is cosmetic (dedup handles overlap).
	all := make([]feed.Source, 0, len(staticSources)+len(dyn))
	all = append(all, staticSources...)
	all = append(all, dyn...)
	return all, nil
}

pipeline := &digest.Pipeline{
	SourceProvider: sourceProvider,
	Store:          store,
	Summarize:      summarizer,
	ChatID:         cfg.TelegramChatID,
}
```

`hc` и `tgLimit` уже локальны в `Run` (`app.go:46`, `app.go:74-77`) — вынести их
объявления выше по функции так, чтобы они были доступны замыкании `sourceProvider`.

### 5.2 Обновить `/sources` листер (заменить `app.go:118-128`)

Показывать id для источников из БД (пользователю нужен id для `/removesource`).
Статические источники id не имеют — печатать их без id, а динамические — с id:

```go
listSources := func(ctx context.Context) (string, error) {
	dbRows, err := store.ListSources(ctx)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("Статические источники (из .env):\n")
	if len(staticSources) == 0 {
		b.WriteString("  (нет)\n")
	}
	for _, src := range staticSources {
		fmt.Fprintf(&b, "  • %s\n", src.Name())
	}
	b.WriteString("\nДинамические источники (команды):\n")
	hasDyn := false
	for _, ds := range dbRows {
		status := "on"
		if !ds.Enabled {
			status = "off"
		}
		fmt.Fprintf(&b, "  #%d [%s] %s (%s)\n", ds.ID, ds.Kind, ds.Ref, status)
		hasDyn = true
	}
	if !hasDyn {
		b.WriteString("  (нет; добавьте через /addsource)\n")
	}
	return b.String(), nil
}
```

---

## 6. Telegram: команды `/addsource` и `/removesource`

### 6.1 Роутинг и извлечение аргумента

`parseCommand` (`router.go:121-134`) отбрасывает аргументы — возвращает только имя
команды. Нужен ОТДЕЛЬНЫЙ разбор аргумента из сырого текста.

**Новая функция** (в `router.go` или новом `commands.go`):

```go
// parseCommandArg returns the trimmed argument portion after the command word.
// For "/addsource https://x" it returns "https://x". Empty when no argument.
func parseCommandArg(text string) string {
	text = strings.TrimSpace(text)
	fields := strings.Fields(text)
	if len(fields) < 2 {
		return ""
	}
	return strings.TrimSpace(strings.Join(fields[1:], " "))
}
```

(для URL/@channel аргумент — одно «слово», но берём хвост целиком на будущее.)

В `switch cmd` (`router.go:62-73`) добавить:

```go
case "addsource":
	b.handleAddSource(ctx, parseCommandArg(u.Message.Text))
case "removesource":
	b.handleRemoveSource(ctx, parseCommandArg(u.Message.Text))
```

Обновить `startText`/`helpText` (`router.go:12-23`) — добавить строки про
`/addsource <url|@channel>` и `/removesource <id>`.

### 6.2 Новый файл `internal/telegram/source_admin.go` — команды и валидация

Bot нуждается в новых зависимостях (доступ к store-CRUD, к валидации через
пробный Collect, к проверке админства). Вводим их через `Option` и поля `Bot`
(как уже сделано для `onDigest`/`listSources`, `bot.go:41-55`).

**Новые поля `Bot`** (`bot.go:28-36`):

```go
addSource    func(ctx context.Context, kind, ref string) (id int64, freshCount int, err error)
removeSource func(ctx context.Context, id int64) (found bool, err error)
```

**Новые Option-конструкторы** (`bot.go`):

```go
func WithSourceAdder(fn func(ctx context.Context, kind, ref string) (int64, int, error)) Option {
	return func(b *Bot) { b.addSource = fn }
}
func WithSourceRemover(fn func(ctx context.Context, id int64) (bool, error)) Option {
	return func(b *Bot) { b.removeSource = fn }
}
```

**Проверка админства бота в канале** — метод на `Bot`, использует `b.api` и `b.ID()`
(библиотека `go-telegram/bot` v1.22.0):

```go
// isBotAdmin reports whether the bot is an administrator (or owner) of @channel.
// channel is WITHOUT leading '@'. Any API error (channel not found, bot not a
// member) is returned so the caller can fall back to public scraping.
func (b *Bot) isBotAdmin(ctx context.Context, channel string) (bool, error) {
	member, err := b.api.GetChatMember(ctx, &bot.GetChatMemberParams{
		ChatID: "@" + channel,
		UserID: b.api.ID(),
	})
	if err != nil {
		return false, err
	}
	return member.Type == models.ChatMemberTypeAdministrator ||
		member.Type == models.ChatMemberTypeOwner, nil
}
```

- `bot.GetChatMemberParams{ChatID any, UserID int64}` — `methods_params.go:638`;
  `ChatID` принимает строку `"@channel"`.
- `b.api.ID()` возвращает user-id бота из токена (`bot.go:105` библиотеки).
- Возвращаемые типы: `models.ChatMemberTypeAdministrator = "administrator"`,
  `models.ChatMemberTypeOwner = "creator"` (`models/chat_member.go:23-24`).

**Хендлеры:**

```go
func (b *Bot) handleAddSource(ctx context.Context, arg string) {
	if b.addSource == nil {
		b.reply(ctx, "Добавление источников недоступно.")
		return
	}
	arg = strings.TrimSpace(arg)
	if arg == "" {
		b.reply(ctx, "Использование: /addsource <URL RSS-ленты | @channel>")
		return
	}

	kind, ref, err := b.detectKind(ctx, arg)
	if err != nil {
		b.reply(ctx, "Не удалось распознать источник: "+err.Error())
		return
	}

	id, fresh, err := b.addSource(ctx, kind, ref)
	if err != nil {
		// addSource does validation (trial Collect) + insert; a validation
		// failure is a normal user error, surfaced as plain text.
		b.reply(ctx, "❌ Источник не добавлен: "+err.Error())
		return
	}
	b.reply(ctx, fmt.Sprintf("✅ Источник #%d добавлен (%s: %s). Свежих постов: %d.",
		id, kind, ref, fresh))
}

func (b *Bot) handleRemoveSource(ctx context.Context, arg string) {
	if b.removeSource == nil {
		b.reply(ctx, "Удаление источников недоступно.")
		return
	}
	id, err := strconv.ParseInt(strings.TrimSpace(arg), 10, 64)
	if err != nil {
		b.reply(ctx, "Использование: /removesource <числовой id> (см. /sources)")
		return
	}
	found, err := b.removeSource(ctx, id)
	if err != nil {
		b.log.Error("remove source failed", "error", err)
		b.reply(ctx, "❌ Внутренняя ошибка при удалении источника.")
		return
	}
	if !found {
		b.reply(ctx, fmt.Sprintf("Источника с id %d нет. /sources — список.", id))
		return
	}
	b.reply(ctx, fmt.Sprintf("✅ Источник #%d отключён.", id))
}
```

**Kind-детекция** — метод `detectKind` на `Bot` (нужен `isBotAdmin`):

```go
// detectKind classifies a raw /addsource argument into (kind, ref).
//   - "http://" / "https://" prefix        -> ("rss", <url>)
//   - "@channel" or bare "channel"         -> "tg_botapi" if bot is admin there,
//                                              else "tg_public"
// ref is normalized: full URL for rss, channel WITHOUT '@' for tg_*.
func (b *Bot) detectKind(ctx context.Context, arg string) (kind, ref string, err error) {
	arg = strings.TrimSpace(arg)
	low := strings.ToLower(arg)
	switch {
	case strings.HasPrefix(low, "http://"), strings.HasPrefix(low, "https://"):
		return "rss", arg, nil
	case strings.HasPrefix(arg, "@"):
		ch := strings.TrimPrefix(arg, "@")
		if ch == "" {
			return "", "", fmt.Errorf("пустое имя канала")
		}
		admin, aerr := b.isBotAdmin(ctx, ch)
		if aerr != nil {
			// Not admin / channel private to the bot: fall back to public scrape.
			b.log.Info("addsource: not admin, using public scrape", "channel", ch, "err", aerr)
			return "tg_public", ch, nil
		}
		if admin {
			return "tg_botapi", ch, nil
		}
		return "tg_public", ch, nil
	default:
		return "", "", fmt.Errorf("ожидается URL (http/https) или @channel")
	}
}
```

> Развилка «@channel → botapi или public»: сперва `isBotAdmin`. Если бот админ →
> `tg_botapi` (посты придут через `ChannelBuffer`, канал должен слать посты боту).
> Любая ошибка или «не админ» → безопасный fallback `tg_public` (скрапинг t.me/s).

### 6.3 Валидация при добавлении — где живёт «пробный Collect»

Валидация — в СЛОЕ СКЛЕЙКИ (`app`), не в `telegram`: там доступны конструкторы
источников и store. `WithSourceAdder` получает замыкание из `app.go`:

```go
// app.go
addSourceFn := func(ctx context.Context, kind, ref string) (int64, int, error) {
	// 1. Build the corresponding feed.Source for a trial collect.
	src, err := trialSource(kind, ref, hc, tgLimit, channelBuf)
	if err != nil {
		return 0, 0, err
	}
	// 2. Trial collect with a bounded context (validation MUST hit the network).
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	items, err := src.Collect(cctx)
	if err != nil {
		return 0, 0, fmt.Errorf("источник недоступен: %w", err)
	}
	if len(items) == 0 {
		return 0, 0, fmt.Errorf("источник не вернул ни одного поста (проверьте адрес/канал)")
	}
	// 3. Persist only after successful validation.
	id, err := store.AddSource(ctx, kind, ref)
	if err != nil {
		return 0, 0, err
	}
	return id, len(items), nil
}
```

**`trialSource`** (в `internal/app/dynamic_sources.go`) строит один источник по
(kind, ref), тем же способом, что `buildDynamicSources`:

```go
func trialSource(kind, ref string, hc *http.Client, tgLimit int, channelBuf *telegram.ChannelBuffer) (feed.Source, error) {
	switch kind {
	case "rss":
		return feed.NewRSSSource(ref, ref, "rss", hc), nil
	case "tg_public":
		return telegram.NewPublicSource("@"+ref, ref, hc, tgLimit), nil
	case "tg_botapi":
		// Trial for a managed channel: cannot pull historic posts via Bot API,
		// so validate "bot is admin" instead of a content collect. Return a
		// source whose Collect drains the buffer (likely empty at add time) —
		// admin-ness was already confirmed in detectKind. Accept with 0 posts.
		if channelBuf == nil {
			return nil, fmt.Errorf("managed-каналы не сконфигурированы")
		}
		return telegram.NewManagedSource("@"+ref, channelBuf), nil
	default:
		return nil, fmt.Errorf("неизвестный вид источника %q", kind)
	}
}
```

> **Важное уточнение по tg_botapi и требованию «≥1 свежий пост».** Bot API НЕ
> отдаёт историю канала — посты приходят только push-ом после добавления. Значит для
> `tg_botapi` пробный `Collect` почти всегда вернёт 0 (буфер пуст в момент
> добавления). Поэтому для `tg_botapi` критерий валидности — НЕ «≥1 пост», а
> «бот подтверждённо админ» (уже проверено в `detectKind` → раз kind стал
> `tg_botapi`, значит `isBotAdmin` вернул true). Реализация: в `addSourceFn`
> ветку `len(items)==0` для `kind=="tg_botapi"` НЕ считать ошибкой — сохранять и
> сообщать «добавлен, посты появятся при новых публикациях». Для `rss` и
> `tg_public` действует обычное правило «0 постов = ошибка, не сохраняем».
> Реализовать это условием: `if len(items)==0 && kind != "tg_botapi" { return err }`.

### 6.4 Wiring в `app.go` (botOpts, заменить `app.go:130-137`)

```go
botOpts := []telegram.Option{
	telegram.WithDigestTrigger(pipeline.Run),
	telegram.WithSourceLister(listSources),
	telegram.WithChannelBuffer(channelBuf), // безусловно
	telegram.WithSourceAdder(addSourceFn),
	telegram.WithSourceRemover(store.DisableSource),
}
```

`store.DisableSource` уже имеет сигнатуру `(ctx, int64) (bool, error)` — совпадает
с `WithSourceRemover`.

---

## 7. Список файлов

| Файл | Действие |
|---|---|
| `internal/storage/sources.go` | НОВЫЙ: тип `Source`, методы `AddSource`/`ListSources`/`DisableSource` |
| `internal/storage/store.go` | добавить 3 метода в интерфейс `Store` |
| `internal/storage/sources_test.go` | НОВЫЙ: unit на CRUD |
| `internal/digest/pipeline.go` | `Sources []feed.Source` → `SourceProvider func(...)`; правка `Run` |
| `internal/digest/pipeline_test.go` | адаптировать существующие тесты под `SourceProvider` |
| `internal/telegram/bot.go` | новые поля + `WithSourceAdder`/`WithSourceRemover` |
| `internal/telegram/router.go` | `parseCommandArg`, ветки `addsource`/`removesource`, тексты help |
| `internal/telegram/source_admin.go` | НОВЫЙ: `handleAddSource`/`handleRemoveSource`/`detectKind`/`isBotAdmin` |
| `internal/telegram/source_admin_test.go` | НОВЫЙ: unit на `detectKind`/`parseCommandArg` |
| `internal/app/app.go` | статические vs динамические; `SourceProvider`; безусловный `channelBuf`; листер с id; `addSourceFn`; botOpts |
| `internal/app/dynamic_sources.go` | НОВЫЙ: `buildDynamicSources`, `trialSource` |

MTProto-файлы и `.env`-слой — без изменений.

---

## 8. Тест-кейсы (для тестировщика на шаге 4a)

### 8.1 `internal/storage/sources_test.go` (unit, реальная временная SQLite `t.TempDir()`)
- `AddSource` возвращает возрастающие id; строка читается `ListSources` с `enabled=true`,
  корректными `kind`/`ref`/непустым `added_at`.
- `ListSources` на пустой таблице → `(nil, nil)`; порядок по возрастанию id.
- `DisableSource` существующего id → `(true, nil)`, после чего `ListSources`
  показывает `Enabled=false`.
- `DisableSource` несуществующего id → `(false, nil)`, без ошибки.
- Дважды `DisableSource` одного id → второй раз всё ещё `(true,nil)` (строка есть,
  RowsAffected=1 т.к. UPDATE переустанавливает 0) — зафиксировать фактическое
  поведение в тесте (допустимо).

### 8.2 `internal/telegram/source_admin_test.go` (unit, table-driven)
- `detectKind` БЕЗ сети для http/https-случаев: `"https://x/feed"` → `("rss","https://x/feed")`;
  `"http://X"` (верхний регистр схемы) → rss; мусор `"foo"` → ошибка; `"@"` → ошибка.
  (`@channel`-ветка требует `b.api`/сети — покрыть отдельно или через фейк; допустимо
  тестировать только «detectKind возвращает tg_public при ошибке isBotAdmin» с
  замоканным методом, если рефакторить `isBotAdmin` в поле-функцию; иначе — ограничиться
  http/мусор-ветками, а @-ветку проверить в ручном приёмочном сценарии).
- `parseCommandArg` table-driven: `"/addsource https://x"`→`"https://x"`;
  `"/addsource"`→`""`; `"/addsource   @ch"`→`"@ch"`; `"/removesource 5"`→`"5"`;
  многословный хвост склеивается.
- `parseCommand` для новых команд возвращает `("addsource",true)` /
  `("removesource",true)` — добавить кейсы в существующий `TestParseCommand`
  (`router_test.go:5-31`).

### 8.3 `internal/digest/pipeline_test.go` (адаптация + новый интеграционный)
- Адаптировать существующие тесты: заменить `Sources: [...]` на
  `SourceProvider: func(ctx)([]feed.Source,error){ return [...], nil }`.
- НОВЫЙ интеграционный кейс «динамический подхват без рестарта»:
  1. Провайдер читает срез, управляемый извне теста (замыкание над указателем на
     `[]feed.Source`).
  2. Первый `Run` с одним fakeSource (item A) → доставлен A.
  3. Добавить в срез второй fakeSource (item B) БЕЗ пересоздания Pipeline.
  4. Второй `Run` → доставлен B (A отфильтрован дедупом). Подтверждает, что
     `SourceProvider` перечитывается на каждом Run.
- НОВЫЙ (опционально, ближе к реальности) через store: провайдер = замыкание над
  реальным `storage.Store` + `buildDynamicSources`; `AddSource(rss-URL)` с фейковым
  RSS через `httptest`; `Run` подхватывает новый источник.

### 8.4 Прогон
- `go build ./...`, `go vet ./...`, `gofmt -l .` (пусто), `go test -race ./...`.
- Интеграционные тесты, пишущие в общую БД, НЕ параллелить (каждый — свой `t.TempDir()`).

---

## 9. Критерий готовности

1. `go build ./...`, `go vet ./...` — зелёные; `gofmt -l .` — пусто;
   `go test -race ./...` — зелёные.
2. `/addsource https://<валидная-rss>` при доступной ленте → ответ «✅ добавлен,
   свежих постов: N (N≥1)»; строка появилась в `sources` с `enabled=1`.
3. `/addsource https://<битый-url>` → ответ «❌ не добавлен: …», в БД строки НЕТ.
4. `/addsource @<публичный-канал>` (бот не админ) → сохранён как `tg_public`.
5. `/addsource @<канал-где-бот-админ>` → сохранён как `tg_botapi` (0 постов при
   добавлении допустимо, сообщение поясняет).
6. `/sources` показывает динамические источники с `#id`, `kind`, `ref`, статусом.
7. `/removesource <id>` существующего → «✅ отключён», после чего источник не
   участвует в прогоне и в `/sources` помечен `off`. Несуществующий id → понятный
   текст, бот не падает.
8. Следующий прогон дайджеста (по `/digest` или расписанию) БЕЗ рестарта включает
   добавленный источник — подтверждено интеграционным тестом §8.3.
9. Ошибки пользователя (битый URL, несуществующий канал) не роняют бота, отвечают
   понятным русским текстом.
10. `internal/mtproto/` и `.env`-слой не изменены; статические источники работают
    как раньше.
