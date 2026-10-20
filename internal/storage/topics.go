package storage

import (
	"context"
	"fmt"
	"time"
)

// Topic is one row of the topics table (Stage 8 B). ID 1 is the seeded
// "Без темы" topic: sources.topic_id NULL is equivalent to topic 1, and all
// reads go through COALESCE(topic_id, 1).
type Topic struct {
	ID          int64
	Name        string
	SourceCount int
}

// ListTopics returns all topics ordered by id with the number of bound sources.
// Sources with topic_id NULL count toward "Без темы" (topic 1).
//
// Semantics note: ListSources returns enabled AND disabled rows, so the count
// here deliberately includes disabled sources too — /topics must agree with
// /sources instead of showing counts the user cannot reproduce.
func (s *SQLiteStore) ListTopics(ctx context.Context) ([]Topic, error) {
	rows, err := s.db.QueryContext(ctx, `
	    SELECT t.id, t.name, COUNT(s.id)
	    FROM topics t
	    LEFT JOIN sources s ON COALESCE(s.topic_id, 1) = t.id
	    GROUP BY t.id, t.name
	    ORDER BY t.id`)
	if err != nil {
		return nil, fmt.Errorf("query topics: %w", err)
	}
	defer rows.Close()

	var out []Topic
	for rows.Next() {
		var t Topic
		if err := rows.Scan(&t.ID, &t.Name, &t.SourceCount); err != nil {
			return nil, fmt.Errorf("scan topic: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate topics: %w", err)
	}
	return out, nil
}

// CreateTopic inserts a topic with a unique name and returns its id.
// Idempotent by name (UNIQUE constraint in migration v3): creating an existing
// topic returns the existing id. The INSERT + SELECT pair runs in one
// transaction (plan §6). suggested-темы создаются этим методом сразу (§4.11);
// неподтверждённая тема остаётся пустой строкой — принимаемый сценарий.
func (s *SQLiteStore) CreateTopic(ctx context.Context, name string) (int64, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		"INSERT OR IGNORE INTO topics(name, created_at) VALUES(?,?)", name, now); err != nil {
		return 0, fmt.Errorf("insert topic %q: %w", name, err)
	}
	var id int64
	if err := tx.QueryRowContext(ctx, "SELECT id FROM topics WHERE name=?", name).Scan(&id); err != nil {
		return 0, fmt.Errorf("select topic id %q: %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return id, nil
}

// SetSourceTopic binds a source to a topic; found=false means no source row
// with that id exists. topic_id NULL ≡ topic 1, so setting topicID=1 is a
// normal assignment. FK enforcement (PRAGMA foreign_keys) is not enabled in
// Open, so the topic's existence is intentionally NOT validated here — the
// command layer checks it against ListTopics before calling.
func (s *SQLiteStore) SetSourceTopic(ctx context.Context, sourceID, topicID int64) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		"UPDATE sources SET topic_id=? WHERE id=?", topicID, sourceID)
	if err != nil {
		return false, fmt.Errorf("set topic %d for source %d: %w", topicID, sourceID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n > 0, nil
}

// UpsertStaticSource registers a config-backed static source (arxiv, hn,
// tg_mtproto, ...) in the DB with origin='static' so it can be given a topic
// and shown in /topics. Called by app.Run on every startup: the row is
// re-enabled and re-stamped as static, but topic_id is deliberately NOT
// touched — a user-set topic survives restarts (plan, Stage B).
func (s *SQLiteStore) UpsertStaticSource(ctx context.Context, kind, ref string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
	    INSERT INTO sources(kind, ref, enabled, added_at, origin) VALUES(?,?,1,?,'static')
	    ON CONFLICT(kind, ref) DO UPDATE SET origin='static', enabled=1`,
		kind, ref, now)
	if err != nil {
		return fmt.Errorf("upsert static source %s/%s: %w", kind, ref, err)
	}
	return nil
}

// StaticRef — координаты (kind, ref) одного статического источника для
// PruneStaticSources (Stage 8 B, ревью M4).
type StaticRef struct {
	Kind string
	Ref  string
}

// PruneStaticSources удаляет static-строки, отсутствующие в keep: источник
// исчез из config (например URL убрали из .env) — его строка иначе навсегда
// осталась бы в БД и засоряла /topics (origin='static' недоступен
// /removesource). Dynamic-строки и keep-строки не трогаются. Одна
// транзакция: SELECT static-строк → diff с keep в Go → DELETE по id.
// Возвращает число удалённых строк.
func (s *SQLiteStore) PruneStaticSources(ctx context.Context, keep []StaticRef) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, "SELECT id, kind, ref FROM sources WHERE origin='static'")
	if err != nil {
		return 0, fmt.Errorf("query static sources: %w", err)
	}
	keepSet := make(map[StaticRef]struct{}, len(keep))
	for _, r := range keep {
		keepSet[r] = struct{}{}
	}
	var orphans []int64
	for rows.Next() {
		var (
			id int64
			r  StaticRef
		)
		if err := rows.Scan(&id, &r.Kind, &r.Ref); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan static source: %w", err)
		}
		if _, ok := keepSet[r]; !ok {
			orphans = append(orphans, id)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("iterate static sources: %w", err)
	}
	// rows закрываем до DELETE: соединение одно (SetMaxOpenConns(1)).
	_ = rows.Close()

	var removed int64
	for _, id := range orphans {
		res, err := tx.ExecContext(ctx, "DELETE FROM sources WHERE id=? AND origin='static'", id)
		if err != nil {
			return 0, fmt.Errorf("delete static source %d: %w", id, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("rows affected: %w", err)
		}
		removed += n
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return removed, nil
}
