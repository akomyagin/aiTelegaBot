// Package storage persists bot state in a local SQLite file (subscriptions,
// seen-items for dedup, digest history, scheduler meta).
//
// Uses database/sql + modernc.org/sqlite (pure Go, no CGO) with hand-written
// SQL and idempotent migrations. Schema: docs/TECHNICAL_PLAN.md §7.
//
// Этап 2: schema + dedup + digest history + scheduler meta.
package storage

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
)

// Store is the persistence port used by the pipeline and scheduler.
type Store interface {
	// FilterUnseen returns only items whose dedup key has not been seen.
	FilterUnseen(ctx context.Context, items []feed.Item) ([]feed.Item, error)
	// MarkSeen records items so they are skipped on future runs.
	MarkSeen(ctx context.Context, items []feed.Item) error
	// SaveDigest stores a delivered digest for audit/history.
	SaveDigest(ctx context.Context, body string, itemCount int) error
	// GetMeta reads a scheduler/meta key; ok is false when the key is absent.
	GetMeta(ctx context.Context, key string) (value string, ok bool, err error)
	// SetMeta upserts a scheduler/meta key.
	SetMeta(ctx context.Context, key, value string) error
	// Close releases the underlying database handle.
	Close() error
}

// SQLiteStore is the SQLite-backed Store.
type SQLiteStore struct {
	db  *sql.DB
	log *slog.Logger
}

// Open opens and migrates the SQLite database. Single-writer (SetMaxOpenConns(1)).
func Open(path string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}
	// Single writer: modernc.org/sqlite serializes fine, but one conn avoids
	// SQLITE_BUSY on the single-user service.
	db.SetMaxOpenConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &SQLiteStore{db: db, log: slog.Default()}, nil
}

// FilterUnseen returns only items whose dedup key is not already in seen_items.
func (s *SQLiteStore) FilterUnseen(ctx context.Context, items []feed.Item) ([]feed.Item, error) {
	if len(items) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(items))
	args := make([]any, len(items))
	for i, it := range items {
		placeholders[i] = "?"
		args[i] = it.DedupKey()
	}
	query := "SELECT dedup_key FROM seen_items WHERE dedup_key IN (" +
		strings.Join(placeholders, ",") + ")"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query seen_items: %w", err)
	}
	defer rows.Close()

	seen := make(map[string]struct{}, len(items))
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("scan dedup_key: %w", err)
		}
		seen[key] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate seen_items: %w", err)
	}

	var out []feed.Item
	for _, it := range items {
		if _, ok := seen[it.DedupKey()]; !ok {
			out = append(out, it)
		}
	}
	return out, nil
}

// MarkSeen records the given items so future runs skip them.
func (s *SQLiteStore) MarkSeen(ctx context.Context, items []feed.Item) error {
	if len(items) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx,
		"INSERT OR IGNORE INTO seen_items(dedup_key, source_id, seen_at) VALUES(?, NULL, ?)")
	if err != nil {
		return fmt.Errorf("prepare insert: %w", err)
	}
	defer stmt.Close()

	now := time.Now().UTC().Format(time.RFC3339)
	for _, it := range items {
		if _, err := stmt.ExecContext(ctx, it.DedupKey(), now); err != nil {
			return fmt.Errorf("insert seen_item: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// SaveDigest stores a delivered digest for audit/history.
func (s *SQLiteStore) SaveDigest(ctx context.Context, body string, itemCount int) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO digests(created_at, item_count, body, delivered) VALUES(?,?,?,1)",
		now, itemCount, body)
	if err != nil {
		return fmt.Errorf("insert digest: %w", err)
	}
	return nil
}

// GetMeta reads a meta key.
func (s *SQLiteStore) GetMeta(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key=?", key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("select meta %q: %w", key, err)
	}
	return value, true, nil
}

// SetMeta upserts a meta key.
func (s *SQLiteStore) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value",
		key, value)
	if err != nil {
		return fmt.Errorf("upsert meta %q: %w", key, err)
	}
	return nil
}

// Close releases the underlying database handle.
func (s *SQLiteStore) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}
