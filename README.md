# aiTelegaBot

Персональный **AI-дайджест** в Telegram. Бот собирает контент из RSS-лент, arXiv,
Hacker News и Telegram-каналов, суммаризует через LLM и присылает одну сжатую
сводку раз в день.

## Возможности (MVP)

- **RSS/Atom/arXiv** — любые ленты через `gofeed`
- **Hacker News** — топ-N историй через Firebase API
- **Telegram-каналы** — три варианта:
  - управляемые (бот добавлен администратором) — через Bot API
  - публичные — best-effort скрапинг `t.me/s/<channel>`
  - **приватные** — через MTProto user-сессию (требует вторичного аккаунта)
- **LLM-суммаризация** — BYOK; без ключа включается offline-режим
- **Расписание** — дайджест в заданное время в вашей TZ; история в SQLite
- **Команды бота:** `/digest` — дайджест по запросу, `/sources` — список источников

---

## Быстрый старт

### 1. Создать бота

Напишите [@BotFather](https://t.me/BotFather), команда `/newbot`. Сохраните токен.

Свой `chat_id` можно узнать через [@userinfobot](https://t.me/userinfobot).

### 2. Установить переменные окружения

```bash
cp .env.example .env
```

Откройте `.env` и заполните минимально необходимые поля:

```dotenv
TELEGRAM_BOT_TOKEN=1234567890:AAxxxxxx   # токен от @BotFather
TELEGRAM_CHAT_ID=123456789               # ваш числовой chat_id
```

Опционально:

```dotenv
LLM_API_KEY=sk-...                       # ключ OpenAI-совместимого API
LLM_BASE_URL=https://api.openai.com/v1   # или http://localhost:11434/v1 для Ollama
LLM_MODEL=gpt-4o-mini

DIGEST_TIME=09:00                        # время дайджеста (HH:MM)
TZ=Europe/Moscow                         # ваша IANA-таймзона
```

### 3. Добавить источники

```dotenv
# RSS/Atom/arXiv (через запятую)
FEED_URLS=https://hnrss.org/frontpage,https://export.arxiv.org/rss/cs.AI

# Hacker News (0 = отключить)
HN_LIMIT=15

# Публичные Telegram-каналы (скрапинг t.me/s)
TG_PUBLIC_CHANNELS=@durov,@golang_news

# Управляемые каналы (бот должен быть администратором)
TG_MANAGED_CHANNELS=@mychannel
```

### 4. Запустить

**Через Docker (рекомендуется для VPS):**

```bash
docker compose up -d --build
docker compose logs -f bot
```

**Локально (для разработки):**

```bash
go run ./cmd/bot
```

---

## Команды бота

| Команда | Действие |
|---|---|
| `/start` | Приветствие |
| `/help` | Справка |
| `/digest` | Запустить дайджест прямо сейчас |
| `/sources` | Список активных источников |

---

## Приватные Telegram-каналы (MTProto)

Для чтения каналов, где вы подписчик, нужна MTProto user-сессия.

> **Важно:** используйте **отдельный вторичный Telegram-аккаунт**, а не основной личный.
> Сессия = полный доступ к аккаунту; при подозрительной активности Telegram может ограничить его.

### Получить app_id и app_hash

1. Зайдите на [my.telegram.org](https://my.telegram.org) под **вторичным аккаунтом**
2. Раздел **API development tools** → создайте приложение
3. Скопируйте `App api_id` и `App api_hash`

### Настроить .env

```dotenv
MTPROTO_APP_ID=1234567
MTPROTO_APP_HASH=abcdef1234567890abcdef1234567890

# Необязательно, но рекомендуется для production:
MTPROTO_SESSION_KEY=$(openssl rand -hex 32)   # 32-байтный AES-ключ шифрования сессии

# Каналы (через запятую, с @ или без)
MTPROTO_CHANNELS=@privatechannel1,@privatechannel2
MTPROTO_LIMIT=20
```

### Выполнить разовый логин

```bash
# Локально:
go run ./cmd/bot login

# В контейнере:
docker compose run --rm bot login
```

Бот спросит номер телефона, код подтверждения и (если включена) 2FA-пароль.
Сессия сохранится в `/data/session.encrypted` на volume — повторный логин не нужен.

---

## Offline-режим

Без `LLM_API_KEY` автоматически включается детерминированный экстрактивный
суммаризатор. Все функции работают — только качество сводки ниже, чем у LLM.

---

## Локальная разработка

```bash
# Сборка и тесты
go build ./...
go test -race ./...
go vet ./...

# Минимальный запуск
export TELEGRAM_BOT_TOKEN=...
export TELEGRAM_CHAT_ID=...
go run ./cmd/bot

# Тест с локальной Ollama
docker compose --profile dev up -d ollama
export LLM_BASE_URL=http://localhost:11434/v1
export LLM_MODEL=llama3.2
go run ./cmd/bot
```

---

## Переменные окружения

| Переменная | Обязательно | Описание | По умолчанию |
|---|---|---|---|
| `TELEGRAM_BOT_TOKEN` | **да** | Токен от @BotFather | — |
| `TELEGRAM_CHAT_ID` | **да** | Chat ID владельца | — |
| `LLM_API_KEY` | нет | Ключ LLM (BYOK); пусто → offline | — |
| `LLM_BASE_URL` | нет | URL OpenAI-совместимого API | `https://api.openai.com/v1` |
| `LLM_MODEL` | нет | Модель | `gpt-4o-mini` |
| `LLM_MAX_RETRIES` | нет | Макс. попыток retry к LLM | `3` |
| `FEED_URLS` | нет | RSS/Atom-ленты, через запятую | — |
| `HN_LIMIT` | нет | Топ-N историй HN; 0 = выкл | `15` |
| `TG_PUBLIC_CHANNELS` | нет | Публичные каналы (`@name,...`) | — |
| `TG_MANAGED_CHANNELS` | нет | Управляемые каналы (бот-админ) | — |
| `TG_SOURCE_LIMIT` | нет | Макс. постов на TG-канал | `20` |
| `DIGEST_TIME` | нет | Время дайджеста `HH:MM` | `09:00` |
| `TZ` | нет | IANA-таймзона | `UTC` |
| `DB_PATH` | нет | Путь к SQLite | `/data/state.db` |
| `MTPROTO_APP_ID` | нет | App ID с my.telegram.org | — |
| `MTPROTO_APP_HASH` | нет | App Hash с my.telegram.org | — |
| `MTPROTO_SESSION_PATH` | нет | Путь к файлу сессии | `/data/session.encrypted` |
| `MTPROTO_SESSION_KEY` | нет | Hex AES-256 ключ; пусто = plaintext | — |
| `MTPROTO_CHANNELS` | нет | Приватные каналы (`@name,...`) | — |
| `MTPROTO_LIMIT` | нет | Макс. постов на MTProto-канал | `20` |

---

## Деплой на VPS

Минимально: VPS с 512 MB RAM и Docker. Европейские провайдеры предпочтительны
(стабильный доступ к `api.telegram.org`).

```bash
# Первый деплой
git clone https://github.com/akomyagin/aiTelegaBot && cd aiTelegaBot
cp .env.example .env           # заполнить токены
docker compose up -d --build

# После обновления кода — пересобрать образ
docker compose up -d --build

# Логи
docker compose logs -f bot

# MTProto логин (один раз, под вторичным аккаунтом)
docker compose run --rm bot login
```

Состояние (SQLite + сессия MTProto) хранится в volume `bot-state` и переживает
рестарт и пересборку образа.

---

## Документация

- [`docs/PLAN.md`](docs/PLAN.md) — видение, этапы, пост-MVP
- [`docs/TECHNICAL_PLAN.md`](docs/TECHNICAL_PLAN.md) — стек, архитектура, схема БД
- [`docs/POST_MVP_PLAN.md`](docs/POST_MVP_PLAN.md) — дорожная карта после MVP

## Лицензия

MIT — см. [`LICENSE`](LICENSE).
