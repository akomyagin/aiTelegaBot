// Link cache and message↔link bindings (Stage 9B, план §4.2/§4.3):
// link_cache — бессрочный кеш по URL (та же статья из второго канала берётся
// из кеша без fetch и без LLM); item_links — привязка извлечённых URL к
// сообщениям (PK (dedup_key, url), ord — порядок в сообщении, читается в
// L3-блоке «Ссылки:»). Паттерны — messages.go (Stage 9A): батч SELECT…IN
// чанками ≤500, запись одной транзакцией, метка времени штампуется внутри.

package storage

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// linkCacheBatchURLs caps the number of URLs per SELECT … IN (…) query —
// messageStateBatchKeys-паттерн ( SQLite host-parameter safety margin).
const linkCacheBatchURLs = 500

// LinkCacheRow is one link_cache row keyed by url.
type LinkCacheRow struct {
	URL       string
	Kind      string // article|course|ad|other|unavailable (§4.3)
	Title     string
	Summary   string
	FetchedAt string // RFC3339 UTC; set on write, filled on read
}

// GetLinkCache returns the cached link rows for the given URLs. Missing URLs
// are simply absent from the map (NOT an error). Queries are chunked to
// ≤linkCacheBatchURLs each (GetMessageStates-паттерн).
func (s *SQLiteStore) GetLinkCache(ctx context.Context, urls []string) (map[string]LinkCacheRow, error) {
	if len(urls) == 0 {
		return map[string]LinkCacheRow{}, nil
	}

	out := make(map[string]LinkCacheRow, len(urls))
	for start := 0; start < len(urls); start += linkCacheBatchURLs {
		end := min(start+linkCacheBatchURLs, len(urls))
		chunk := urls[start:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, u := range chunk {
			placeholders[i] = "?"
			args[i] = u
		}
		query := `SELECT url, kind, title, summary, fetched_at
		         FROM link_cache WHERE url IN (` + strings.Join(placeholders, ",") + ")"

		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("query link_cache: %w", err)
		}
		for rows.Next() {
			var r LinkCacheRow
			if err := rows.Scan(&r.URL, &r.Kind, &r.Title, &r.Summary, &r.FetchedAt); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan link_cache: %w", err)
			}
			out[r.URL] = r
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("iterate link_cache: %w", err)
		}
		rows.Close()
	}
	return out, nil
}

// UpsertLinkCache writes a batch of link_cache rows in ONE transaction
// (UpsertMessageState-паттерн, план §6). fetched_at is stamped internally
// (RFC3339 UTC now) — поле FetchedAt входа игнорируется; any error rolls the
// whole batch back. PK = url: повторная вставка того же URL — update, не дубль.
func (s *SQLiteStore) UpsertLinkCache(ctx context.Context, rows []LinkCacheRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO link_cache(url, kind, title, summary, fetched_at)
		 VALUES(?,?,?,?,?)
		 ON CONFLICT(url) DO UPDATE SET
		    kind=excluded.kind, title=excluded.title, summary=excluded.summary,
		    fetched_at=excluded.fetched_at`)
	if err != nil {
		return fmt.Errorf("prepare link_cache upsert: %w", err)
	}
	defer stmt.Close()

	now := time.Now().UTC().Format(time.RFC3339)
	for _, r := range rows {
		if _, err := stmt.ExecContext(ctx, r.URL, r.Kind, r.Title, r.Summary, now); err != nil {
			return fmt.Errorf("upsert link_cache %q: %w", r.URL, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit link_cache batch: %w", err)
	}
	return nil
}

// ItemLink is one item_links row: a URL extracted from a message (dedup_key)
// with its kind (§4.3, вызов E; course/ad попадают сюда БЕЗ fetch) and Ord —
// порядок ссылки в сообщении (детерминированный рендер блока «Ссылки:»).
type ItemLink struct {
	DedupKey string
	URL      string
	Kind     string
	Ord      int
}

// UpsertItemLinks writes message↔link bindings in ONE transaction
// (UpsertMessageState-паттерн). PK = (dedup_key, url): повторная вставка той
// же пары — update (kind/ord перезаписываются), не дубль.
func (s *SQLiteStore) UpsertItemLinks(ctx context.Context, links []ItemLink) error {
	if len(links) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO item_links(dedup_key, url, kind, ord)
		 VALUES(?,?,?,?)
		 ON CONFLICT(dedup_key, url) DO UPDATE SET
		    kind=excluded.kind, ord=excluded.ord`)
	if err != nil {
		return fmt.Errorf("prepare item_links upsert: %w", err)
	}
	defer stmt.Close()

	for _, l := range links {
		if _, err := stmt.ExecContext(ctx, l.DedupKey, l.URL, l.Kind, l.Ord); err != nil {
			return fmt.Errorf("upsert item_link (%q, %q): %w", l.DedupKey, l.URL, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit item_links batch: %w", err)
	}
	return nil
}

// ListItemLinks returns one message's links ordered by ord ASC (порядок
// ссылок в сообщении — детерминированный рендер). Unknown dedup_key → пустой
// результат, НЕ ошибка (GetDigest-паттерн «crafted id не роняет viewer»).
func (s *SQLiteStore) ListItemLinks(ctx context.Context, dedupKey string) ([]ItemLink, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT dedup_key, url, kind, ord FROM item_links WHERE dedup_key=? ORDER BY ord ASC", dedupKey)
	if err != nil {
		return nil, fmt.Errorf("query item_links %q: %w", dedupKey, err)
	}
	defer rows.Close()

	var out []ItemLink
	for rows.Next() {
		var l ItemLink
		if err := rows.Scan(&l.DedupKey, &l.URL, &l.Kind, &l.Ord); err != nil {
			return nil, fmt.Errorf("scan item_links: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate item_links: %w", err)
	}
	return out, nil
}
