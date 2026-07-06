// Package storage persists bot state in a local SQLite file (subscriptions,
// seen-items for dedup, digest history, scheduler meta).
//
// Uses database/sql + modernc.org/sqlite (pure Go, no CGO) with hand-written
// SQL and idempotent migrations. Schema: docs/TECHNICAL_PLAN.md §7.
//
// Stage 0: interface + stub — real store lands in Этап 2 (schema/dedup) and
// Этап 4 (digest history / scheduler meta).
package storage

import (
	"context"

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
	// Close releases the underlying database handle.
	Close() error
}

// SQLiteStore is the SQLite-backed Store. Stage 0: stub.
type SQLiteStore struct {
	path string
}

// Open opens (and, later, migrates) the SQLite database. Stage 0: stub.
func Open(path string) (*SQLiteStore, error) {
	return &SQLiteStore{path: path}, nil
}

func (s *SQLiteStore) FilterUnseen(ctx context.Context, items []feed.Item) ([]feed.Item, error) {
	_ = ctx
	return items, nil
}

func (s *SQLiteStore) MarkSeen(ctx context.Context, items []feed.Item) error {
	_ = ctx
	_ = items
	return nil
}

func (s *SQLiteStore) SaveDigest(ctx context.Context, body string, itemCount int) error {
	_ = ctx
	_ = body
	_ = itemCount
	return nil
}

func (s *SQLiteStore) Close() error { return nil }
