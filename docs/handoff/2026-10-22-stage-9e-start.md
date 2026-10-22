# Handoff 2026-10-22 — Этап 9: 9A–9D смерджены, старт 9E (медиа)

Заменяет `2026-10-22-stage-9d-start.md`.

## Состояние

- **9A** (классификация + message_state + v5) — PR #15, master `8a787ad`.
- **9B** (внешние ссылки: linkfetch, E/F, link_cache, ↳-конкатенация) —
  PR #16, master `eb3377e`.
- **9C** (семантический дедуп: вызов B, dropped_dup, degraded-политика) —
  PR #17, master `70c1c3b`.
- **9D** (4-уровневая навигация + агрегация C/D + v6 sections_json) —
  PR #19, master `5e0000f`. Навигация: выпуск (TOC+overall) → тема (T:,
  RenderTopicView, кнопки-имена топиков) → топик (S:<did>:<sid>, sid=0 — все)
  → сообщение (M:<itemID>: full_text, «Ссылки:», кнопки 🖼 P: /↗ URL /◀ S:);
  legacy-alias'ы L1:/L2:/L3: Этапа 8 работают. P: — ЗАГЛУШКА ack «Медиа
  недоступно», PDEL — ack-only без deleteMessage; CallbackResult.Send объявлен,
  всегда nil — всё это активируется в 9E.
- Прогоны: build/vet/gofmt чистые, `go test -race` 9/9 пакетов ok на каждом
  подэтапе. «Итог 9A/9B/9C/9D» (отклонения, решения диспетчера, техдолг с
  триггерами) — в `docs/plans/stage-9-bottom-up-digest.md`.
- **Деплой на VPS НЕ выполнялся** — 9A–9D накоплены в master (каждый автономен;
  можно задеплоить как после 9E, так и сейчас). Перед деплоем: дамп `state.db`
  (миграции v5/v6 идемпотентны, откат не предусмотрен) + ручной сценарий 9D
  (оглавление → тема → топик → сообщение → назад; рестарт → старые кнопки,
  включая legacy Этапа 8) и дедуп-скрипт прод-БД, если v2 ещё не применялась
  (`2026-10-20-stage-8-plan-gate.md`).
- **Мандат «мерж и продолжай» ДЕЙСТВУЕТ** до конца Этапа 9 (9E — последний
  подэтап): после зелёного прогона и закрытия находок ревью — мердж без
  отдельного вопроса, чекпойнты выводить.

## Дальше: подэтап 9E — медиа

Спека: план, «Подэтап 9E» (строки ~1021–1077) + §4.5/§4.5.1/§4.5.2 (источники
картинок по kind, MTProto download→re-upload→file_id→deleteMessage: стриминг
на диск, лимит 3/прогон ОБЩИЙ на run — счётчик в pipeline, FloodWait через
существующий middleware, RAM 458MB — не держать байты в памяти; показ:
отдельное sendPhoto-сообщение с caption ≤1024 и «✖ Закрыть» PDEL →
deleteMessage; текстовое сообщение edit'ом в фото НЕ превращается).

Флаги из «Итог 9D» и кода для 9E:
1. `CallbackResult.Send` (telegram/bot.go) — тип объявлен в 9D-2a, обработка
   НЕ реализована (handleDrillDown его не читает) — механика sendPhoto живёт
   в 9E; контракт: seam возвращает Send, telegram выполняет.
2. P:-ветка в telegram-диспетчере — заглушка ack «Медиа недоступно»
   (dialogs.go) + дубль-защита в digest.NewDrillDown (mediaUnavailableAck) —
   активировать ОБА места согласованно.
3. media_kind/media_ref уже пишутся в digest_items и message_state (колонки
   v5), но ИСТОЧНИКИ их не заполняют: tg_botapi — ChannelPost.Photo file_id
   сейчас игнорируется (botapi_source/buffer); tg_public — img из photo_wrap
   при скрапинге t.me/s; RSS — enclosure/media-контент URL; MTProto — resolver
   в pipeline (шаг 4 между c и d, лимит 3/прогон).
4. «🖼»-маркер в L2 и кнопка «🖼 Фото» P: в M-view рендерятся УЖЕ (9D-1) при
   media_ref≠'' — достаточно начать заполнять media_ref.
5. peer-Title fallback только для Ref «-100X» (техдолг 9D.4) — при желании
   закрывается в 9E при media-снапшоте GetPeer.
6. Caption sendPhoto: ≤1024 рун, ParseMode HTML — весь динамический текст
   через EscapeHTML, guard по байтам/рунам (урок A2 H1: лимиты БАЙТОВЫЕ).
7. Техдолг 9D.1: Client/Offline.Summarize (Этап 1) мёртв — удалить при
   правке internal/llm в 9E, если llm правится.

Пайплайн стандартный: кодинг Agent `model:"opus"` по разделу плана (при
необходимости дробить на 2 прогона: «источники+resolver» → «показ
sendPhoto+PDEL»; отчёты — текстом или в /tmp) → тесты (Sonnet) + ревью (Opus)
параллельно, промпт ревью «сообщи всё с критичностью и уверенностью» → триаж
file:line в основной сессии → цикл фиксов (≤3) → финальный прогон сам →
«Итог 9E» + критерий готовности ЭТАПА (§3 плана) + KV-нота + память →
commit/push/PR → мердж по мандату → Этап 9 завершён по коду; деплой —
отдельная задача (см. выше).

## Что читать холодной сессии

1. Этот файл.
2. `docs/plans/stage-9-bottom-up-digest.md`: «Подэтап 9E», §4.5.x, §6, §7,
   §3 (критерий этапа), «Итог 9D» (контракты: NavView, CallbackResult.Send,
   P:/PDEL-заглушки, media-колонки), при необходимости «Итог 9A/9B/9C».
3. `.claude/skills/go-telegram-bot-dev/SKILL.md` + CLAUDE.md репо.
4. Память сессии: `stage-9-bottom-up-digest.md` (статус/мандат/флаги),
   `vps_deployment.md` (лимит RAM, обходной путь сборки — для MTProto-
   resolver'а и деплоя).
5. KV-ноты (workspace myProjects): 9A 9bdf2549, 9B c43584a6, 9D 460a5695.
