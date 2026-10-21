package storage

import (
	"context"
	"database/sql"
	"fmt"
)

// migrations is the ordered list of schema DDL. Index i applies to reach
// user_version i+1. Never reorder or edit an already-shipped migration; append.
var migrations = []string{
	// v1: initial schema (docs/TECHNICAL_PLAN.md §7).
	`
	CREATE TABLE IF NOT EXISTS sources (
	    id       INTEGER PRIMARY KEY,
	    kind     TEXT NOT NULL,
	    ref      TEXT NOT NULL,
	    enabled  INTEGER NOT NULL DEFAULT 1,
	    added_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS seen_items (
	    dedup_key TEXT PRIMARY KEY,
	    source_id INTEGER,
	    seen_at   TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS digests (
	    id         INTEGER PRIMARY KEY,
	    created_at TEXT NOT NULL,
	    item_count INTEGER NOT NULL,
	    body       TEXT NOT NULL,
	    delivered  INTEGER NOT NULL DEFAULT 0
	);

	CREATE TABLE IF NOT EXISTS meta (
	    key   TEXT PRIMARY KEY,
	    value TEXT NOT NULL
	);
	`,
	// v2 (Stage 8 A1): MTProto peer cache + uniqueness of (kind, ref) in
	// sources. The UNIQUE index fails loudly if a legacy DB carries duplicate
	// (kind, ref) rows — that is intentional (docs/plans/stage-8, A1): dedup
	// is a separate manual query, migrations never swallow such conflicts.
	`
	CREATE TABLE IF NOT EXISTS mtproto_peers (
	    peer_id     INTEGER PRIMARY KEY,          -- MTProto id (X of Bot API "-100X")
	    access_hash INTEGER NOT NULL,             -- sensitive: never logged
	    title       TEXT NOT NULL DEFAULT '',
	    username    TEXT NOT NULL DEFAULT '',
	    updated_at  TEXT NOT NULL
	);
	CREATE UNIQUE INDEX IF NOT EXISTS sources_kind_ref ON sources(kind, ref);
	`,
	// v3 (Stage 8 B): topics + source attribution. topics is seeded with id=1
	// "Без темы"; sources.topic_id NULL ≡ topic 1 — all reads go through
	// COALESCE(topic_id, 1). origin separates config-backed static sources
	// ('static', registered by app.Run at startup) from user-added ones
	// ('dynamic'). ALTER TABLE ADD COLUMN with REFERENCES is legal in SQLite
	// because the column defaults to NULL; FK enforcement is not enabled in
	// Open (no PRAGMA foreign_keys), consistent with the rest of the schema.
	`
	CREATE TABLE IF NOT EXISTS topics (
	    id         INTEGER PRIMARY KEY,
	    name       TEXT NOT NULL UNIQUE,
	    created_at TEXT NOT NULL
	);
	INSERT OR IGNORE INTO topics(id, name, created_at) VALUES (1, 'Без темы', datetime('now'));
	ALTER TABLE sources ADD COLUMN origin   TEXT NOT NULL DEFAULT 'dynamic';
	ALTER TABLE sources ADD COLUMN topic_id INTEGER REFERENCES topics(id);
	`,
	// v4 (Stage 8 C): topic digests — one TOC message per run. digests gain
	// topic_id (NULL on legacy rows ≡ topic 1 — all reads go through
	// COALESCE(topic_id, 1)) and run_id (Unix seconds of the run; 0 = «легаси,
	// не прогон»: ListRunDigests(0) always returns nothing). digest_items
	// stores the per-item drill-down rows rendered on demand (L1/L2/L3 views
	// are not persisted as messages); source_id NULL = source unknown (Go 0 →
	// NULL). ALTER TABLE ADD COLUMN with REFERENCES is legal in SQLite
	// because the column defaults to NULL; FK enforcement is not enabled in
	// Open, consistent with the rest of the schema.
	`
	ALTER TABLE digests ADD COLUMN topic_id INTEGER REFERENCES topics(id);
	ALTER TABLE digests ADD COLUMN run_id   INTEGER NOT NULL DEFAULT 0;
	CREATE TABLE IF NOT EXISTS digest_items (
	    id           INTEGER PRIMARY KEY,
	    digest_id    INTEGER NOT NULL REFERENCES digests(id),
	    source_id    INTEGER,
	    dedup_key    TEXT NOT NULL,
	    title        TEXT NOT NULL DEFAULT '',
	    url          TEXT NOT NULL DEFAULT '',
	    tg_peer      TEXT NOT NULL DEFAULT '',
	    tg_msg_id    INTEGER NOT NULL DEFAULT 0,
	    published_at TEXT NOT NULL DEFAULT '',
	    snippet      TEXT NOT NULL DEFAULT '',
	    summary      TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX IF NOT EXISTS digest_items_digest ON digest_items(digest_id);
	`,
	// v5 (Этап 9, план §4.2): состояние обработки сообщений, кеш ссылок и их
	// привязка к сообщениям, общее саммари прогона, полный текст/класс/медиа у
	// digest_items. message_state — кеш обработки: упавший прогон не теряет
	// LLM-работу, а следующий не переспрашивает LLM о том же (ключ = dedup_key,
	// тот же, что в seen_items — «эволюция»: seen_items остаётся таблицей
	// ДОСТАВКИ (что уже было показано), message_state — таблицей ОБРАБОТКИ (что
	// уже классифицировано/саммаризовано). digest_items остаётся per-run
	// снапшотом для навигации. Новые колонки digest_items имеют DEFAULT '' —
	// legacy-строки читаются как «нет полного текста/класса/медиа».
	`
	CREATE TABLE IF NOT EXISTS message_state (
	    dedup_key  TEXT PRIMARY KEY,
	    source_id  INTEGER,
	    topic_id   INTEGER,
	    class      TEXT NOT NULL DEFAULT '',        -- §4.1; '' = не классифицировано
	    cluster_id TEXT NOT NULL DEFAULT '',        -- '<runID>:<n>'; '' = не в кластере
	    is_primary INTEGER NOT NULL DEFAULT 1,      -- 0 = дубль, не показывается
	    summary    TEXT NOT NULL DEFAULT '',        -- саммари сообщения (1–2 предложения)
	    media_kind TEXT NOT NULL DEFAULT '',        -- '' | 'photo'
	    media_ref  TEXT NOT NULL DEFAULT '',        -- tg file_id ИЛИ http(s) URL картинки
	    status     TEXT NOT NULL DEFAULT 'new',     -- new|done|dropped_ad|dropped_dup|failed
	    updated_at TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS link_cache (
	    url        TEXT PRIMARY KEY,
	    kind       TEXT NOT NULL DEFAULT '',        -- article|course|ad|other|unavailable
	    title      TEXT NOT NULL DEFAULT '',
	    summary    TEXT NOT NULL DEFAULT '',
	    fetched_at TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS item_links (
	    dedup_key TEXT NOT NULL,
	    url       TEXT NOT NULL,
	    kind      TEXT NOT NULL DEFAULT '',         -- класс ссылки (§4.3)
	    ord       INTEGER NOT NULL DEFAULT 0,       -- порядок в сообщении
	    PRIMARY KEY (dedup_key, url)
	);
	CREATE TABLE IF NOT EXISTS runs (
	    run_id     INTEGER PRIMARY KEY,             -- Unix-секунды, как digests.run_id
	    summary    TEXT NOT NULL DEFAULT '',        -- общее саммари выпуска (L0)
	    created_at TEXT NOT NULL
	);
	ALTER TABLE digest_items ADD COLUMN full_text TEXT NOT NULL DEFAULT '';  -- до 8000 рун
	ALTER TABLE digest_items ADD COLUMN class     TEXT NOT NULL DEFAULT '';
	ALTER TABLE digest_items ADD COLUMN media_kind TEXT NOT NULL DEFAULT '';
	ALTER TABLE digest_items ADD COLUMN media_ref  TEXT NOT NULL DEFAULT '';
	`,
}

// migrate applies pending migrations starting from the current PRAGMA
// user_version. Each migration runs in its own transaction; user_version is
// bumped in the same transaction so a partial upgrade never advances the version.
func migrate(ctx context.Context, db *sql.DB) error {
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}

	for i := version; i < len(migrations); i++ {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %d: %w", i+1, err)
		}
		// PRAGMA user_version doesn't accept a bound parameter.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("set user_version %d: %w", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", i+1, err)
		}
	}
	return nil
}
