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
