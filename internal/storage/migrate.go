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
