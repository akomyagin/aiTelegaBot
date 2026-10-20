package storage

import (
	"context"
	"fmt"
	"time"
)

// Source is one row of the sources table (dynamic, DB-backed sources added at
// runtime via /addsource or by forwarding a message, Stage 8). Kind is one of
// feed.Item.Kind values managed here: "rss" | "tg_botapi" | "tg_public" |
// "tg_mtproto" (private channels/supergroups, ref in Bot API "-100X" form).
// "arxiv"/"hn" are NOT created via commands.
type Source struct {
	ID      int64
	Kind    string
	Ref     string // RSS URL, or channel username WITHOUT leading '@'
	Enabled bool
	AddedAt string // RFC3339 UTC
	Origin  string // 'dynamic' (added via commands) | 'static' (config-backed, Stage 8 B)
	TopicID int64  // effective topic: COALESCE(topic_id, 1) — NULL reads as 1 ("Без темы")
}

// AddSource inserts an enabled source and returns its new id.
func (s *SQLiteStore) AddSource(ctx context.Context, kind, ref string) (int64, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO sources(kind, ref, enabled, added_at) VALUES(?,?,1,?)",
		kind, ref, now)
	if err != nil {
		return 0, fmt.Errorf("insert source: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("last insert id: %w", err)
	}
	return id, nil
}

// AddSourceIfNotExists inserts an enabled source unless one with the same
// (kind, ref) already exists (UNIQUE index sources_kind_ref, migration v2).
// Outcomes, all inside one transaction:
//   - created=true: a new enabled row was inserted;
//   - reactivated=true: the existing row was disabled (/removesource) and is
//     re-enabled here with a refreshed added_at — re-adding a removed source
//     is a first-class user path (Stage 8 review, finding H1);
//   - both false: an enabled row already exists.
//
// id always refers to the (kind, ref) row. The UNIQUE index also closes the
// race between two parallel additions.
func (s *SQLiteStore) AddSourceIfNotExists(ctx context.Context, kind, ref string) (id int64, created bool, reactivated bool, err error) {
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, false, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		"INSERT INTO sources(kind, ref, enabled, added_at) VALUES(?,?,1,?) ON CONFLICT(kind, ref) DO NOTHING",
		kind, ref, now)
	if err != nil {
		return 0, false, false, fmt.Errorf("insert source: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, false, false, fmt.Errorf("rows affected: %w", err)
	}

	if n > 0 {
		id, err = res.LastInsertId()
		if err != nil {
			return 0, false, false, fmt.Errorf("last insert id: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return 0, false, false, fmt.Errorf("commit: %w", err)
		}
		return id, true, false, nil
	}

	var enabled int
	if err := tx.QueryRowContext(ctx,
		"SELECT id, enabled FROM sources WHERE kind=? AND ref=?", kind, ref).Scan(&id, &enabled); err != nil {
		return 0, false, false, fmt.Errorf("select source: %w", err)
	}
	if enabled == 0 {
		if _, err := tx.ExecContext(ctx,
			"UPDATE sources SET enabled=1, added_at=? WHERE id=?", now, id); err != nil {
			return 0, false, false, fmt.Errorf("reactivate source %d: %w", id, err)
		}
		reactivated = true
	}
	if err := tx.Commit(); err != nil {
		return 0, false, false, fmt.Errorf("commit: %w", err)
	}
	return id, false, reactivated, nil
}

// ListSources returns all sources ordered by id (enabled and disabled).
func (s *SQLiteStore) ListSources(ctx context.Context) ([]Source, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, kind, ref, enabled, added_at, origin, COALESCE(topic_id, 1) FROM sources ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("query sources: %w", err)
	}
	defer rows.Close()
	var out []Source
	for rows.Next() {
		var src Source
		var enabled int
		if err := rows.Scan(&src.ID, &src.Kind, &src.Ref, &enabled, &src.AddedAt, &src.Origin, &src.TopicID); err != nil {
			return nil, fmt.Errorf("scan source: %w", err)
		}
		src.Enabled = enabled != 0
		out = append(out, src)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sources: %w", err)
	}
	return out, nil
}

// DisableSource sets enabled=0 for the given id. Returns (false, nil) when no
// row with that id exists (so the command can report "нет источника с id N").
func (s *SQLiteStore) DisableSource(ctx context.Context, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, "UPDATE sources SET enabled=0 WHERE id=?", id)
	if err != nil {
		return false, fmt.Errorf("disable source %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n > 0, nil
}
