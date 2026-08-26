package storage

import (
	"context"
	"fmt"
	"time"
)

// Source is one row of the sources table (dynamic, DB-backed sources added at
// runtime via /addsource). Kind is one of feed.Item.Kind values managed here:
// "rss" | "tg_botapi" | "tg_public". "arxiv"/"hn"/"tg_mtproto" are NOT created
// via commands.
type Source struct {
	ID      int64
	Kind    string
	Ref     string // RSS URL, or channel username WITHOUT leading '@'
	Enabled bool
	AddedAt string // RFC3339 UTC
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

// ListSources returns all sources ordered by id (enabled and disabled).
func (s *SQLiteStore) ListSources(ctx context.Context) ([]Source, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, kind, ref, enabled, added_at FROM sources ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("query sources: %w", err)
	}
	defer rows.Close()
	var out []Source
	for rows.Next() {
		var src Source
		var enabled int
		if err := rows.Scan(&src.ID, &src.Kind, &src.Ref, &enabled, &src.AddedAt); err != nil {
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
