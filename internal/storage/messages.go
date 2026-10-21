// Message processing state (Stage 9A, план §4.2): message_state is the
// PROCESSING cache (what has already been classified/summarized), while
// seen_items stays the DELIVERY table (what has already been shown). A failed
// run does not lose LLM work; the next run reads the cache instead of asking
// the LLM about the same items again.

package storage

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// messageStateBatchKeys caps the number of dedup keys per SELECT … IN (…)
// query (план §9A: плейсхолдеры генерируются по len, ≤500 ключей на запрос).
const messageStateBatchKeys = 500

// MessageState is one message_state row keyed by dedup_key.
type MessageState struct {
	DedupKey  string
	SourceID  int64 // 0 ↔ NULL
	TopicID   int64 // 0 ↔ NULL
	Class     string
	ClusterID string
	IsPrimary bool
	Summary   string
	MediaKind string
	MediaRef  string
	Status    string // new|done|dropped_ad|dropped_dup|failed
	UpdatedAt string // RFC3339 UTC; set on write, filled on read
}

// GetMessageStates returns the cached processing states for the given dedup
// keys. Missing keys are simply absent from the map (NOT an error). Queries
// are chunked to ≤500 keys each (SQLite host-parameter safety margin).
func (s *SQLiteStore) GetMessageStates(ctx context.Context, keys []string) (map[string]MessageState, error) {
	if len(keys) == 0 {
		return map[string]MessageState{}, nil
	}

	out := make(map[string]MessageState, len(keys))
	for start := 0; start < len(keys); start += messageStateBatchKeys {
		end := min(start+messageStateBatchKeys, len(keys))
		chunk := keys[start:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, k := range chunk {
			placeholders[i] = "?"
			args[i] = k
		}
		query := `SELECT dedup_key, COALESCE(source_id, 0), COALESCE(topic_id, 0), class, cluster_id,
		         is_primary, summary, media_kind, media_ref, status, updated_at
		         FROM message_state WHERE dedup_key IN (` + strings.Join(placeholders, ",") + ")"

		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("query message_state: %w", err)
		}
		var isPrimary int
		for rows.Next() {
			var st MessageState
			if err := rows.Scan(&st.DedupKey, &st.SourceID, &st.TopicID, &st.Class, &st.ClusterID,
				&isPrimary, &st.Summary, &st.MediaKind, &st.MediaRef, &st.Status, &st.UpdatedAt); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan message_state: %w", err)
			}
			st.IsPrimary = isPrimary != 0
			out[st.DedupKey] = st
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("iterate message_state: %w", err)
		}
		rows.Close()
	}
	return out, nil
}

// UpsertMessageState writes a batch of processing states in ONE transaction
// (план §6: батч UpsertMessageState — одна транзакция). updated_at is stamped
// internally (RFC3339 UTC now); any error rolls the whole batch back.
func (s *SQLiteStore) UpsertMessageState(ctx context.Context, states []MessageState) error {
	if len(states) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO message_state(dedup_key, source_id, topic_id, class, cluster_id, is_primary,
		    summary, media_kind, media_ref, status, updated_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(dedup_key) DO UPDATE SET
		    source_id=excluded.source_id, topic_id=excluded.topic_id, class=excluded.class,
		    cluster_id=excluded.cluster_id, is_primary=excluded.is_primary, summary=excluded.summary,
		    media_kind=excluded.media_kind, media_ref=excluded.media_ref, status=excluded.status,
		    updated_at=excluded.updated_at`)
	if err != nil {
		return fmt.Errorf("prepare message_state upsert: %w", err)
	}
	defer stmt.Close()

	now := time.Now().UTC().Format(time.RFC3339)
	for _, st := range states {
		isPrimary := 0
		if st.IsPrimary {
			isPrimary = 1
		}
		if _, err := stmt.ExecContext(ctx, st.DedupKey, nilIfZero(st.SourceID), nilIfZero(st.TopicID),
			st.Class, st.ClusterID, isPrimary, st.Summary, st.MediaKind, st.MediaRef, st.Status, now); err != nil {
			return fmt.Errorf("upsert message_state %q: %w", st.DedupKey, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit message_state batch: %w", err)
	}
	return nil
}

// MarkSeenBatch marks dedup keys seen in ONE transaction (план §4.2: реклама и
// дубли получают seen сразу при классификации, до доставки — они никогда не
// показываются, терять их при сбое доставки нечего). source_id stays NULL:
// the key-only callers (dropped ad/dup) carry no provenance here — same as
// MarkSeen. Existing rows are kept (INSERT OR IGNORE).
func (s *SQLiteStore) MarkSeenBatch(ctx context.Context, keys []string) error {
	if len(keys) == 0 {
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
		return fmt.Errorf("prepare seen insert: %w", err)
	}
	defer stmt.Close()

	now := time.Now().UTC().Format(time.RFC3339)
	for _, k := range keys {
		if _, err := stmt.ExecContext(ctx, k, now); err != nil {
			return fmt.Errorf("insert seen_item %q: %w", k, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit seen batch: %w", err)
	}
	return nil
}
