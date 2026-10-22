# Handoff 2026-10-23 — Этап 9 ЗАКРЫТ ПО КОДУ (9A–9E); следующая задача — деплой на VPS

Заменяет `2026-10-22-stage-9e-start.md`.

## Состояние

- **9A** (классификация + message_state + v5) — PR #15, master `8a787ad`.
- **9B** (внешние ссылки: linkfetch, E/F, link_cache, ↳-конкатенация) —
  PR #16, master `eb3377e`.
- **9C** (семантический дедуп: вызов B, dropped_dup, degraded-политика) —
  PR #17, master `70c1c3b`.
- **9D** (4-уровневая навигация + агрегация C/D + v6 sections_json) —
  PR #19, master `5e0000f`.
- **9E** (медиа: извлечение у источников, MTProto-resolver
  download→re-upload→file_id→deleteMessage с бюджетом 3/прогон, показ
  sendPhoto + активация P:/PDEL и CallbackResult.Send) — PR #21.
  Ревью: 1 high (резка готового HTML-caption — закрыт fitEscapedBytes) /
  2 medium / 5 low / 6 info; «Итог 9E» (отклонения + 6 строк техдолга с
  триггерами) — в `docs/plans/stage-9-bottom-up-digest.md`.
- Прогоны каждого подэтапа: build/vet/gofmt чистые, `go test -race` 9/9
  пакетов ok. Финальный прогон 9E — основной сессией после фиксов.
- **Деплой на VPS НЕ выполнялся — 9A–9E накоплены в master** (каждый
  подэтап автономен, деплой один на все).

## Следующая задача: деплой 9A–9E на VPS (спросить пользователя отдельно —
## мандат «мерж и продолжай» действовал только до конца Этапа 9)

Чек-лист (из handoff 9E-старта + памяти `vps_deployment.md`):

1. **Дамп `state.db` на сервере** до миграций (`docker exec` + cp из volume
   `bot-state`): v5/v6 идемпотентны, но откат не предусмотрен.
2. Сборка образа **локально** (VPS ~458 MB RAM — `docker compose build` на
   сервере падает OOM при компиляции gotd/td/tg): `docker compose build bot`
   → `docker save aitelegabot-bot -o bot.tar` → `scp` → `docker load` →
   `docker compose up -d` БЕЗ `--build`.
3. Дедуп-скрипт прод-БД, если v2 ещё не применялась
   (`2026-10-20-stage-8-plan-gate.md`).
4. **Ручной сценарий 9D**: оглавление → тема → топик → сообщение → назад;
   рестарт → старые кнопки (включая legacy L1:/L2:/L3: Этапа 8) работают.
5. **Ручной сценарий 9E** (критерий этапа §3): канал с картинками
   (tg_public или managed) → L3 → «🖼 Фото» → фото отдельным сообщением,
   «✖ Закрыть» удаляет; при настроенном MTProto — фото приватного канала
   (re-upload ≤3/прогон, в логах нет FloodWait; `MT_MEDIA_REUPLOAD_MAX`
   default 3, ≤0 → 3).
6. Логи после первого прогона: нет Error A-батча + Warn «media cache write
   skipped» (техдолг 9E.2 — каскад повторных re-upload'ов).

## Что читать холодной сессии

1. Этот файл.
2. `docs/plans/stage-9-bottom-up-digest.md`: «Итог 9E» (свежее) + «Итоги
   9A–9D» (отклонения не переоткрывать), §3 (критерий этапа), §6–§7.
3. `.claude/skills/go-telegram-bot-dev/SKILL.md` + CLAUDE.md репо.
4. Память сессии: `stage-9-bottom-up-digest.md`, `vps_deployment.md`
   (обходной путь сборки — критично для деплоя).
5. KV-ноты (workspace myProjects): 9A 9bdf2549, 9B c43584a6, 9D 460a5695,
   9E — создана 2026-10-23 (id см. в памяти сессии).

## После деплоя

Этап 9 завершён полностью. Дальше — `docs/POST_MVP_PLAN.md` (что именно
брать — спросить пользователя).
