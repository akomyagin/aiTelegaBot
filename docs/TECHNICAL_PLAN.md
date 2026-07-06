# TECHNICAL_PLAN — детальный технический план `aiTelegaBot`

> Технический компаньон к [`PLAN.md`](./PLAN.md). Что вне MVP —
> в [`POST_MVP_PLAN.md`](./POST_MVP_PLAN.md).
>
> Терминология этапов — **«Этап N»** (не «недели»). Этапы сгруппированы в две
> фазы: Фаза 1 (Этапы 0–5) — ядро MVP без MTProto; Фаза 2 (Этапы 6–7) —
> приватные Telegram-каналы через MTProto.

---

## 1. Вводные и ограничения

- Соло-разработчик, реализация с AI-ассистентом; приоритет — **обучение Go**
  (нагружать язык и stdlib, а не SDK). Экономическая целесообразность необязательна.
- **Первый сервисный рантайм портфеля:** бот работает постоянно (long-polling +
  крон), значит — реальная, но дешёвая инфраструктура (VPS ~$5–10/мес).
- **Go 1.22+** (нужны `slices`, `maps`, `log/slog`, `errors.Join`, современный
  `net/http`).
- **BYOK** — ключ LLM у пользователя; секреты никогда не в git.
- **Один пользователь** в MVP (сам автор): нет мультитенантности, нет очередей,
  нет внешней БД — только локальный SQLite-файл на volume.
- Работа с двумя чувствительными идентичностями: bot-токен (ограниченный скоуп) и
  **MTProto user-сессия** (полный доступ к аккаунту — см. §5).

## 2. Технический стек

| Слой | Выбор | Обоснование |
|---|---|---|
| Язык | Go 1.22+ | Приоритет обучения; stdlib закрывает большую часть нужд |
| Telegram Bot API | `github.com/go-telegram/bot` | Zero-dependency, современный (context-aware, generics-хендлеры), покрывает long-polling/webhook — обучающий, но не «магический» SDK. Обоснование выбора — §4 |
| Telegram MTProto | `github.com/gotd/td` | Единственная зрелая Go-реализация MTProto; Go-моно-стек вместо TS-сайдкара — §5 |
| RSS/Atom | `github.com/mmcdole/gofeed` | De-facto стандарт разбора RSS/Atom/JSON-Feed в Go; ручной разбор форматов не даёт обучающей ценности, а багов много |
| arXiv / HN | поверх `gofeed` (arXiv Atom API) + `net/http` к HN Firebase API | HN — ручной `net/http`+`encoding/json` (обучающий); arXiv — Atom-лента через gofeed |
| HTTP к LLM | `net/http` (stdlib), **без SDK** | Осознанно вручную — тренировка клиента, retry+backoff, таймаутов |
| JSON | `encoding/json` (stdlib) | Ответы LLM, HN API, конфиг переопределений |
| Планировщик | `time.Timer`/`time.Ticker` (stdlib) в MVP; `robfig/cron/v3` при cron-выражениях | Начинаем со stdlib (обучающе); cron-библиотека — только когда понадобится cron-синтаксис. §6 |
| Хранилище | SQLite через `modernc.org/sqlite` (**без CGO**) | Чистый Go → простой multi-stage Docker без C-тулчейна; файл на volume. §7 |
| Доступ к БД | `database/sql` (stdlib) + ручной SQL | Без ORM — обучающий эффект, полный контроль над схемой/миграциями |
| Логи | `log/slog` (stdlib) | Структурные логи; `--verbose`/`LOG_LEVEL` → debug |
| Конфиг | env-переменные + опц. YAML (`gopkg.in/yaml.v3`) | Секреты через env (12-factor, дружит с docker-compose); несекретные настройки — YAML |
| Тесты | stdlib `testing` + table-driven + `httptest` | Идиоматика Go; фейки провайдеров для интеграционного яруса |

**Сознательное ограничение зависимостей:** HTTP к LLM и HN — stdlib `net/http`;
SQL — stdlib `database/sql`. Внешние библиотеки берём там, где ручная реализация
несоразмерна и не обучает (`gofeed` — разбор десятка форматов RSS; `gotd/td` —
криптопротокол MTProto; `go-telegram/bot` — тонкая типобезопасная обёртка над
Bot API).

## 3. Архитектура и структура проекта

```
aiTelegaBot/
├── cmd/
│   └── bot/
│       └── main.go              # тонкий main: context+signal, config.Load, app.Run
├── internal/
│   ├── config/                  # env/YAML → struct Config; валидация; выбор offline-режима
│   │   └── config.go
│   ├── feed/                    # сбор веб-источников → []Item (RSS/Atom/arXiv/HN)
│   │   ├── source.go            # интерфейс Source, тип Item, дедуп-ключ
│   │   ├── rss.go               # gofeed-адаптер (RSS/Atom/arXiv)
│   │   └── hackernews.go        # net/http к HN Firebase API
│   ├── llm/                     # HTTP-клиент к OpenAI-совместимому API + offline
│   │   ├── client.go            # net/http, таймауты, retry+backoff+jitter
│   │   ├── summarize.go         # сборка промпта дайджеста, разбор ответа
│   │   └── offline.go           # детерминированный экстрактивный суммаризатор
│   ├── telegram/                # доставка и команды (Bot API) + чтение источников
│   │   ├── bot.go               # go-telegram/bot: long-polling, роутинг команд
│   │   ├── commands.go          # /start, /digest, /help, /sources
│   │   └── source_botapi.go     # чтение управляемых каналов/групп + t.me/s скрапинг
│   ├── mtproto/                 # ФАЗА 2: user-сессия через gotd/td (приватные каналы)
│   │   ├── client.go            # gotd/td: логин, session storage, FloodWait-backoff
│   │   └── source_mtproto.go    # чтение приватных каналов → []Item
│   ├── digest/                  # оркестрация пайплайна collect→summarize→render
│   │   └── pipeline.go
│   ├── scheduler/               # крон: запуск пайплайна по расписанию
│   │   └── scheduler.go
│   ├── storage/                 # SQLite: миграции, дедуп seen-items, история дайджестов
│   │   ├── store.go             # database/sql, open, миграции
│   │   └── queries.go           # seen-items, digest history, subscriptions
│   └── app/                     # сборка зависимостей, Run(ctx) — склейка всего
│       └── app.go
├── testdata/                    # фикстуры RSS/HN/Telegram, golden-рендеры дайджеста
├── Dockerfile                   # multi-stage build статического бинарника
├── docker-compose.yml           # сервис bot + volume state + опц. ollama
├── go.mod
└── README.md
```

**Почему `internal/`:** всё ядро в `internal/`, чтобы Go-компилятор **запрещал**
внешний импорт — это приложение, а не библиотека. **`pkg/` не заводим на старте**:
публичный API появится только при реальном спросе («не строй библиотеку, пока не
просят»).

### 3.1 Поток данных (один прогон дайджеста)

```
scheduler tick (или /digest)
  → digest.Pipeline.Run(ctx)
      → feed.Collect(ctx, sources)        // RSS/Atom/arXiv/HN → []Item
      → telegram.CollectSources(ctx, …)   // Bot API-каналы + t.me/s
      → mtproto.CollectSources(ctx, …)    // Фаза 2: приватные каналы
      → storage.FilterUnseen(items)       // дедуп по ключу (URL/GUID/msgID)
      → llm.Summarize(ctx, items)         // retry/backoff, offline-fallback
      → digest.Render(summary)            // Markdown/HTML для Telegram
      → telegram.Deliver(ctx, chatID, text)
      → storage.MarkSeen(items) + storage.SaveDigest(summary)
```

Отмена по SIGINT/SIGTERM — единый `context.Context` из `signal.NotifyContext`,
протянутый сквозь HTTP, крон и Telegram-клиент (graceful shutdown сервиса).

## 4. Telegram Bot API — выбор библиотеки

**Выбор: `github.com/go-telegram/bot`.** Причины, релевантные цели «написание +
обучение Go»:

1. **Zero-dependency, современный дизайн** — context-aware хендлеры, generics,
   поддержка long-polling и webhook. Это тонкая типобезопасная обёртка над HTTP
   Bot API, а не тяжёлый фреймворк, скрывающий механику.
2. **Активно поддерживается** и покрывает актуальную схему Bot API — в отличие от
   исторически популярного `go-telegram-bot-api/telegram-bot-api`, у которого
   вялый темп обновлений и устаревшие места.
3. **Обучающий баланс** — не пишем разбор Bot API руками (это не даёт ценного
   Go-навыка сверх обычного `net/http`), но и не прячемся за «магией»: роутинг
   команд, апдейт-луп и graceful-shutdown реализуем сами.

Компромисс инкапсулируется: весь `go-telegram/bot` спрятан за интерфейсом
`telegram.Deliverer`/`telegram.SourceReader` в `internal/telegram` — если позже
понадобится webhook или другая библиотека, меняется одна реализация.

## 5. MTProto: `gotd/td` (Go) vs TS-сайдкар (GramJS) — решение

**Выбор: остаёмся полностью на Go, используем `github.com/gotd/td`.**

Почему Go-моно-стек, а не сайдкар на TypeScript+GramJS:

1. **Прямая служба цели «учить Go».** MTProto-часть — самая нетривиальная в
   проекте; реализовать её на Go = максимальная обучающая нагрузка. Сайдкар на TS
   вынес бы самое интересное во второй язык и второй рантайм.
2. **`gotd/td` достаточно зрелая** для личного проекта с низким объёмом запросов:
   активная поддержка, полное покрытие MTProto-схемы, встроенные абстракции для
   session storage и обработки rate-limit (`floodwait`/`ratelimit`-миддлвары в
   `gotd/contrib`). Для одного пользователя, читающего десяток каналов раз в день,
   её возможностей с запасом хватает.
3. **Один рантайм, один Dockerfile, один процесс.** Сайдкар потребовал бы Node
   в образе, IPC/HTTP между Go и TS, двойной набор зависимостей и обновлений —
   несоразмерная сложность для pet-проекта на одного.
4. **Обновляемость под протокол-мидж (июнь 2026).** Session-string v2 и
   гранулярные `FLOOD_*_WAIT_X` — это ровно то, что мейнтейнеры библиотек (и
   `gotd/td`, и GramJS) закрывают апдейтом за 1–3 недели. Мы **принимаем апдейт
   зависимости**, а не пишем низкоуровневый клиент — так что этот риск одинаков
   для обоих вариантов и не даёт TS преимущества.

**Когда TS-сайдкар был бы оправдан (не наш случай):** если бы Go-реализация MTProto
была незрелой/заброшенной. Это не так — поэтому Go-моно-стек предпочтителен.

### 5.1 Риски MTProto и их митигация (Фаза 2)

| Риск | Суть | Митигация |
|---|---|---|
| Бан/ограничение реального аккаунта | Сессия — **личный** аккаунт, не бот с ограниченным скоупом | **Отдельный вторичный Telegram-аккаунт** только под бота; не основной личный |
| **FloodWait** | >~30 сообщений/сек/пользователь уже триггерит лимиты; в 2026 — гранулярные `FLOOD_PREMIUM_WAIT_X`/`FLOOD_PEER_WAIT_X` | `gotd/contrib` floodwait+ratelimit миддлвары; **exponential backoff + jitter**; консервативная частота опроса (раз в день, не поллинг) |
| **Смена протокола (июнь 2026)** | session-string v2, депрекация части методов | Пинуем свежую `gotd/td`, не пишем свой низкоуровневый клиент; вынесли MTProto за интерфейс — обновление изолировано |
| Компрометация session-файла | Файл сессии = **полный доступ к аккаунту** | Хранить **вне git** (в `.gitignore`), на volume; **шифровать** session (ключ из env, не в образе); права `0600` |
| Датацентр-IP | Постоянно залогиненная сессия с IP VPS — доп.сигнал анти-абьюза | Консервативное поведение, вторичный аккаунт, готовность к повторному логину |

**Следствие для роадмапа:** вся ценность MVP (Фаза 1) достигается **без** MTProto.
Фаза 2 включается осознанно, только когда нужны приватные каналы.

## 6. Планировщик

- **MVP (Этап 4):** stdlib `time.Timer`/`time.Ticker` + расчёт «следующего запуска»
  на заданное локальное время (например, 09:00). Обучающе: работа с `time`,
  таймзонами (`time.LoadLocation`), пересчётом после каждого прогона, отменой по
  `context`.
- **Когда понадобится cron-синтаксис** (несколько расписаний, `*/30 * * * *`) —
  подключаем `github.com/robfig/cron/v3`. Не тащим его раньше, чем реально нужен.
- Планировщик **не** должен пропускать/дублировать запуск при рестарте: последний
  успешный прогон фиксируется в SQLite; при старте — «пропущен ли сегодняшний слот».

## 7. Схема данных (SQLite)

Файл на volume (`/data/state.db`). Доступ — `database/sql` + `modernc.org/sqlite`
(чистый Go, без CGO). Миграции — простые встроенные SQL-скрипты, применяются при
старте (idempotent `CREATE TABLE IF NOT EXISTS`), версия схемы в `PRAGMA user_version`.

```sql
-- источники подписки (что опрашивать)
CREATE TABLE IF NOT EXISTS sources (
    id          INTEGER PRIMARY KEY,
    kind        TEXT NOT NULL,   -- 'rss' | 'arxiv' | 'hn' | 'tg_botapi' | 'tg_public' | 'tg_mtproto'
    ref         TEXT NOT NULL,   -- URL ленты / имя канала / chat_id
    enabled     INTEGER NOT NULL DEFAULT 1,
    added_at    TEXT NOT NULL
);

-- уже виденные элементы (дедуп между запусками)
CREATE TABLE IF NOT EXISTS seen_items (
    dedup_key   TEXT PRIMARY KEY,  -- URL/GUID/(channel:msgID)
    source_id   INTEGER,
    seen_at     TEXT NOT NULL
);

-- история отправленных дайджестов
CREATE TABLE IF NOT EXISTS digests (
    id          INTEGER PRIMARY KEY,
    created_at  TEXT NOT NULL,
    item_count  INTEGER NOT NULL,
    body        TEXT NOT NULL,     -- отправленный текст (для аудита/повтора)
    delivered   INTEGER NOT NULL DEFAULT 0
);

-- служебное: последний успешный слот планировщика, версии и т.п.
CREATE TABLE IF NOT EXISTS meta (
    key         TEXT PRIMARY KEY,
    value       TEXT NOT NULL
);
```

Дедуп-ключ (`internal/feed` / `internal/storage`): для веб-источников — канонический
URL или `<guid>`; для Telegram — `tg:<channel>:<messageID>`. Ретеншн `seen_items`
(например, удалять старше N дней) — простой периодический `DELETE`.

## 8. Рантайм и упаковка

- **`Dockerfile`** — multi-stage: builder на `golang:1.22` (`CGO_ENABLED=0
  go build -o /bot ./cmd/bot`), рантайм на `gcr.io/distroless/static` или `alpine`.
  Чистый Go + `modernc.org/sqlite` без CGO → статический бинарник, минимальный образ.
- **`docker-compose.yml`** — **впервые в портфеле нужен для продукта, не только
  для dev**: сервис `bot` (из Dockerfile) + именованный volume `bot-state` под
  `/data` (SQLite, session-файл) + опциональный сервис `ollama` (профиль `dev`)
  для локального теста LLM без ключа. Секреты — через env/`.env` (в `.gitignore`).
- На VPS: `docker compose up -d --build`, `restart: unless-stopped`, логи в
  `journald`/`docker logs`.

## 9. Разбивка по Этапам (действия / deliverable / критерий)

Каждый этап реализуется по dev-workflow из `CLAUDE.md` (ветка от `master` → анализ
→ код → ревью → тесты → актуализация docs → PR).

### Фаза 1 — ядро MVP (без MTProto)

**Этап 0 — Bootstrap** (реализовано этим bootstrap-проходом)
- *Действия:* `go mod init github.com/akomyagin/aiTelegaBot`; каркас `cmd/bot` +
  заглушки `internal/{config,feed,llm,telegram,scheduler,storage}` (+ `digest`,
  `app`); `docs/` (PLAN/TECHNICAL_PLAN/POST_MVP_PLAN); `CLAUDE.md`; SKILL.md;
  `Dockerfile`; `docker-compose.yml`; `README.md`.
- *Deliverable:* компилируемый каркас без бизнес-логики.
- *Критерий:* **`go build ./...` зелёный**; структура и docs на месте; секреты в
  `.gitignore`.

**Этап 1 — Доставка в Telegram + команды**
- *Действия:* `internal/telegram/bot.go` на `go-telegram/bot` (long-polling,
  graceful shutdown по ctx); команды `/start`, `/help`, `/digest` (заглушка
  «дайджест скоро»), `/sources`; интерфейс `Deliverer` + отправка сообщения в
  личный чат по `chat_id` из конфига.
- *Deliverable:* бот отвечает на команды и шлёт сообщение самому себе.
- *Критерий:* реальный бот-токен → `/digest` присылает тестовое сообщение; юнит-
  тесты роутинга команд с фейковым API.

**Этап 2 — Веб-источники + модель контента**
- *Действия:* тип `feed.Item` (title, url, source, published, summary/text);
  `rss.go` (gofeed для RSS/Atom и arXiv Atom API), `hackernews.go` (`net/http` к
  HN Firebase API: topstories → item); нормализация в `Item`; дедуп-ключ; запись
  `sources`/`seen_items` в SQLite.
- *Deliverable:* `feed.Collect(ctx, sources)` возвращает свежие `[]Item` без
  дубликатов между запусками.
- *Критерий:* table-driven тесты парсинга на фикстурах `testdata/`; повторный
  прогон не возвращает уже виденное.

**Этап 3 — LLM-суммаризация + BYOK + offline**
- *Действия:* `internal/llm/client.go` (ручной `net/http`, таймаут+ctx, retry с
  экспоненциальным backoff+jitter, типизированные ретраебельные/фатальные ошибки,
  ключ **не логируется**); `summarize.go` (сборка промпта из `[]Item`, разбор
  ответа); `offline.go` (детерминированный экстрактивный суммаризатор без сети);
  BYOK через env `LLM_API_KEY`, авто-offline при пустом ключе.
- *Deliverable:* `llm.Summarize(ctx, items)` → сводка; без ключа — offline.
- *Критерий:* `httptest` сценарий `429→200` (retry); отсутствие ключа в
  stdout/stderr при ошибке; детерминизм offline (один вход → один выход).

**Этап 4 — Планировщик + сборка дайджеста**
- *Действия:* `internal/scheduler` (stdlib `time`, ежедневный слот в заданной TZ,
  пересчёт после прогона, отмена по ctx, «не пропустить/не дублировать» через
  `meta`); `internal/digest/pipeline.go` (collect→summarize→render→deliver);
  Markdown/HTML-рендер дайджеста; запись `digests`.
- *Deliverable:* по расписанию (и по `/digest`) приходит собранный дайджест из
  веб-источников; история в SQLite.
- *Критерий:* end-to-end прогон offline присылает дайджест; рестарт сервиса не
  теряет/не дублирует слот; golden-тест рендера.

**Этап 5 — Telegram-источники через Bot API** (конец Фазы 1)
- *Действия:* `source_botapi.go` — чтение управляемых каналов/групп (бот-админ)
  через Bot API; best-effort скрапинг `t.me/s/<channel>` для публичных каналов
  (`net/http`+HTML-парсинг, помечен как хрупкий, деградирует мягко); нормализация
  в `Item`, дедуп `tg:<channel>:<msgID>`.
- *Deliverable:* Telegram-источники (без приватных) попадают в дайджест.
- *Критерий:* реальный управляемый канал читается; поломка `t.me/s`-скрапинга не
  роняет прогон (лог + пропуск).

### Фаза 2 — приватные Telegram-каналы через MTProto

**Этап 6 — MTProto-логин + безопасное хранение сессии**
- *Действия:* `internal/mtproto/client.go` на `gotd/td`: интерактивная авторизация
  user-сессии (**вторичный аккаунт**, api_id/api_hash из env), session storage на
  volume **вне git**, **шифрование** session (ключ из env), права `0600`;
  разовый CLI-режим логина (`bot login` или отдельный флаг).
- *Deliverable:* успешный логин, зашифрованная сессия переживает рестарт.
- *Критерий:* сессия не в git, не в образе; повторный старт не требует релогина;
  документирован разовый логин.

**Этап 7 — Чтение приватных каналов + FloodWait-дисциплина** (конец MVP)
- *Действия:* `source_mtproto.go` — опрос приватных каналов/чатов
  (`messages.getHistory`), нормализация в `Item`; подключение
  floodwait+ratelimit миддлваров `gotd/contrib`; собственный exponential
  backoff+jitter поверх; консервативная частота; graceful-обработка
  `FLOOD_*_WAIT_X`.
- *Deliverable:* приватные каналы попадают в дайджест наравне с остальными.
- *Критерий:* приватный канал вторичного аккаунта читается; при FloodWait бот ждёт
  по backoff, не спамит retry, не падает; частота опроса заведомо ниже лимитов.

## 10. Тестовая стратегия

- **Юнит:** парсеры (`feed`), сборка промпта, дедуп-ключи, рендер дайджеста
  (golden), расчёт слота планировщика — table-driven.
- **Интеграционный ярус (не только мок-HTTP):** детерминированные **фейки**
  провайдеров (LLM-offline, фейковый Telegram-`Deliverer`, фикстурный feed-Source)
  прогоняют **реальный пайплайн** `digest.Pipeline.Run` поверх временной SQLite —
  проверяют дедуп, историю, идемпотентность слота.
- **`httptest.Server`** для retry-логики LLM и HN-клиента (429→200, таймаут).
- Где конкурентность (параллельный сбор источников) — `-race`.
- LLM-сеть и MTProto-сеть за интерфейсами; в тестах — фейки/offline, реальные
  вызовы не в CI.
