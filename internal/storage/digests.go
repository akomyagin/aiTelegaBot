// Topic digests: digests rows with topic attribution + per-item drill-down
// rows (Stage 8 C, plan §4.3). Write order per run: InsertDigest
// (delivered=0) → TOC delivery whose buttons carry the digestID →
// MarkDelivered (ONE transaction: delivered=1 + digest_items + seen_items).
// A delivery failure leaves the row delivered=0 and the items unseen — the
// next run picks them up. L2/L3 views are NOT stored: they are rendered on
// demand from digest_items.

package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
)

// digestItemSnippetRunes caps the stored snippet (first N runes of Text).
const digestItemSnippetRunes = 200

// digestItemFullTextRunes caps the stored full_text (Этап 9, план §4.2: до
// 8000 рун).
const digestItemFullTextRunes = 8000

// digestSelect is the shared column list for digests reads. topic_id is read
// through COALESCE(topic_id, 1): legacy NULL ≡ «Без темы».
const digestSelect = "SELECT id, run_id, COALESCE(topic_id, 1), created_at, item_count, body, delivered"

// Digest is one persisted topic digest (a digests row).
type Digest struct {
	ID        int64
	RunID     int64  // Unix seconds of the run; 0 = legacy «не прогон»
	TopicID   int64  // read via COALESCE(topic_id, 1)
	CreatedAt string // RFC3339 UTC
	ItemCount int
	Body      string // ready HTML L1 topic header (RenderTopicBody), stored at insert
	Delivered bool
}

// DigestItem is one drill-down row of a digest (digest_items).
type DigestItem struct {
	ID int64 // 0 on write; filled on read
	// DigestID заполняется ТОЛЬКО в GetDigestItem (Stage 9D, M:-view: кнопка
	// «◀ Назад» S:<did>:<sid> берёт did из самой строки digest_items, план
	// §4.6); List*-методы оставляют 0 — их вызывающим digest_id известен из
	// запроса. При записи игнорируется (MarkDelivered вставляет свой digestID).
	DigestID    int64
	SourceID    int64 // 0 ↔ NULL (written as a nil argument)
	DedupKey    string
	Title       string
	URL         string
	TgPeer      string // username (no @) or "-100X"; tg_* kinds only
	TgMsgID     int64
	PublishedAt string // RFC3339 UTC
	Snippet     string // first 200 runes of Text
	Summary     string // LLM per-item line (for L2 in Stage 8 D)
	// Этап 9 (§4.2): legacy-строки читаются с '' (DEFAULT) и рендерятся
	// деградированно (snippet вместо full_text).
	FullText  string // first 8000 runes of Text
	Class     string // §4.1 taxonomy; '' = не классифицировано
	MediaKind string // '' | 'photo'
	MediaRef  string // tg file_id ИЛИ http(s) URL картинки
}

// NewDigestItem builds a digest_items row from a feed item. TgPeer/TgMsgID
// are filled only for tg_* kinds (Item.Source of tg_mtproto is already in
// "-100X" form — A1); an unparseable message id yields 0. SourceID is the
// provenance stamped by digest.taggedSource (§4.2); 0 = unknown → NULL.
// Этап 9: class приходит из message_state (пустая строка = не
// классифицировано), full_text — Text, обрезанный до 8000 рун, media_ref —
// MediaFileID, при пустом — MediaURL (план §4.5).
func NewDigestItem(it feed.Item, summary, class string) DigestItem {
	di := DigestItem{
		SourceID:    it.SourceID,
		DedupKey:    it.DedupKey(),
		Title:       it.Title,
		URL:         it.URL,
		PublishedAt: it.Published.UTC().Format(time.RFC3339),
		Snippet:     truncateRunes(it.Text, digestItemSnippetRunes),
		Summary:     summary,
		FullText:    truncateRunes(it.Text, digestItemFullTextRunes),
		Class:       class,
		MediaKind:   it.MediaKind,
	}
	switch {
	case it.MediaFileID != "":
		di.MediaRef = it.MediaFileID
	case it.MediaURL != "":
		di.MediaRef = it.MediaURL
	}
	if strings.HasPrefix(it.Kind, "tg_") {
		di.TgPeer = strings.TrimPrefix(it.Source, "@")
		if id, err := strconv.ParseInt(it.ID, 10, 64); err == nil {
			di.TgMsgID = id
		}
	}
	return di
}

// InsertDigest creates a digests row with delivered=0 and returns its id
// (plan §4.3: delivery gates persistence).
func (s *SQLiteStore) InsertDigest(ctx context.Context, runID, topicID int64, body string, itemCount int) (int64, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	// topicID 0 → NULL (review C1 L2): reads COALESCE(topic_id,1), so a
	// literal 0 would bypass the «Без темы» normalization — defense even
	// though pipeline Run maps 0→1 itself.
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO digests(created_at, item_count, body, delivered, topic_id, run_id) VALUES(?,?,?,0,?,?)",
		now, itemCount, body, nilIfZero(topicID), runID)
	if err != nil {
		return 0, fmt.Errorf("insert digest: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("digest last insert id: %w", err)
	}
	return id, nil
}

// MarkDelivered flips delivered=1, inserts the digest_items rows and marks
// the items seen — ALL in one transaction; any error rolls everything back
// and surfaces. seen_items.source_id gets DigestItem.SourceID (0 → NULL —
// more informative than MarkSeen's unconditional NULL, plan C).
func (s *SQLiteStore) MarkDelivered(ctx context.Context, digestID int64, items []DigestItem) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, "UPDATE digests SET delivered=1 WHERE id=?", digestID)
	if err != nil {
		return fmt.Errorf("mark delivered: %w", err)
	}
	// Defense (review C1 L1): a missing digest row would silently no-op the
	// UPDATE and still insert digest_items "nowhere" (FK not enforced).
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("mark delivered: digest %d does not exist", digestID)
	}

	itemStmt, err := tx.PrepareContext(ctx,
		`INSERT INTO digest_items(digest_id, source_id, dedup_key, title, url, tg_peer, tg_msg_id, published_at, snippet, summary, full_text, class, media_kind, media_ref)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return fmt.Errorf("prepare digest_items insert: %w", err)
	}
	defer itemStmt.Close()

	seenStmt, err := tx.PrepareContext(ctx,
		"INSERT OR IGNORE INTO seen_items(dedup_key, source_id, seen_at) VALUES(?,?,?)")
	if err != nil {
		return fmt.Errorf("prepare seen insert: %w", err)
	}
	defer seenStmt.Close()

	now := time.Now().UTC().Format(time.RFC3339)
	for _, it := range items {
		if _, err := itemStmt.ExecContext(ctx, digestID, nilIfZero(it.SourceID), it.DedupKey,
			it.Title, it.URL, it.TgPeer, it.TgMsgID, it.PublishedAt, it.Snippet, it.Summary,
			it.FullText, it.Class, it.MediaKind, it.MediaRef); err != nil {
			return fmt.Errorf("insert digest_item %q: %w", it.DedupKey, err)
		}
		if _, err := seenStmt.ExecContext(ctx, it.DedupKey, nilIfZero(it.SourceID), now); err != nil {
			return fmt.Errorf("insert seen_item %q: %w", it.DedupKey, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit mark delivered: %w", err)
	}
	return nil
}

// GetDigest reads one digest. ok=false (NOT an error) when the id is unknown
// — crafted callback ids must not fail the viewer (planner decision, plan C).
func (s *SQLiteStore) GetDigest(ctx context.Context, digestID int64) (Digest, bool, error) {
	row := s.db.QueryRowContext(ctx, digestSelect+" FROM digests WHERE id=?", digestID)
	d, delivered, err := scanDigest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Digest{}, false, nil
	}
	if err != nil {
		return Digest{}, false, err
	}
	d.Delivered = delivered != 0
	return d, true, nil
}

// digestItemSelect is the shared column list for digest_items reads
// (source_id through COALESCE(…, 0): NULL ↔ «0 = unknown»).
const digestItemSelect = `SELECT id, COALESCE(source_id, 0), dedup_key, title, url, tg_peer, tg_msg_id, published_at, snippet, summary, full_text, class, media_kind, media_ref
	 FROM digest_items`

// ListDigestItems returns a digest's drill-down rows ordered by id ASC.
func (s *SQLiteStore) ListDigestItems(ctx context.Context, digestID int64) ([]DigestItem, error) {
	rows, err := s.db.QueryContext(ctx, digestItemSelect+" WHERE digest_id=? ORDER BY id ASC", digestID)
	if err != nil {
		return nil, fmt.Errorf("query digest_items: %w", err)
	}
	defer rows.Close()
	return scanDigestItems(rows)
}

// ListDigestItemsBySource returns one source's drill-down rows of a digest,
// ordered by id ASC (Stage 8 D, план §D — выборка для L3). sourceID=0 means
// «all rows of the digest»: L3:<did>:0 renders the common list without a
// source filter (plan D: кнопка «слишком много источников» → L3:<did>:0), so
// it delegates to ListDigestItems. Rows with source_id NULL never match a
// non-zero sourceID (SQL NULL semantics) — unknown provenance stays out of
// per-source views but is present in the 0-list.
func (s *SQLiteStore) ListDigestItemsBySource(ctx context.Context, digestID, sourceID int64) ([]DigestItem, error) {
	if sourceID == 0 {
		return s.ListDigestItems(ctx, digestID)
	}
	rows, err := s.db.QueryContext(ctx, digestItemSelect+" WHERE digest_id=? AND source_id=? ORDER BY id ASC",
		digestID, sourceID)
	if err != nil {
		return nil, fmt.Errorf("query digest_items by source: %w", err)
	}
	defer rows.Close()
	return scanDigestItems(rows)
}

// scanDigestItems reads the digestItemSelect column list from an open *sql.Rows
// (caller closes). Empty result → nil slice, NOT an error.
func scanDigestItems(rows *sql.Rows) ([]DigestItem, error) {
	var out []DigestItem
	for rows.Next() {
		var it DigestItem
		if err := rows.Scan(&it.ID, &it.SourceID, &it.DedupKey, &it.Title, &it.URL,
			&it.TgPeer, &it.TgMsgID, &it.PublishedAt, &it.Snippet, &it.Summary,
			&it.FullText, &it.Class, &it.MediaKind, &it.MediaRef); err != nil {
			return nil, fmt.Errorf("scan digest_item: %w", err)
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate digest_items: %w", err)
	}
	return out, nil
}

// ListRunDigests returns one run's digests ordered by topic_id ASC.
// runID=0 means «легаси, не прогон» and always yields an empty result
// (migration v4: legacy rows keep run_id 0).
func (s *SQLiteStore) ListRunDigests(ctx context.Context, runID int64) ([]Digest, error) {
	if runID == 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, digestSelect+" FROM digests WHERE run_id=? ORDER BY topic_id ASC", runID)
	if err != nil {
		return nil, fmt.Errorf("query digests by run: %w", err)
	}
	defer rows.Close()

	var out []Digest
	for rows.Next() {
		d, delivered, err := scanDigest(rows)
		if err != nil {
			return nil, err
		}
		d.Delivered = delivered != 0
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate digests by run: %w", err)
	}
	return out, nil
}

// rowScanner is the Scan side of both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanDigest reads the digestSelect column list. sql.ErrNoRows propagates
// wrapped (errors.Is still matches).
func scanDigest(row rowScanner) (Digest, int, error) {
	var d Digest
	var delivered int
	if err := row.Scan(&d.ID, &d.RunID, &d.TopicID, &d.CreatedAt, &d.ItemCount, &d.Body, &delivered); err != nil {
		return Digest{}, 0, fmt.Errorf("scan digest: %w", err)
	}
	return d, delivered, nil
}

// nilIfZero maps the Go «0 = unknown» convention to SQL NULL.
func nilIfZero(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// truncateRunes returns s cut to at most n runes (local helper — the llm
// copy is unexported; plan C accepts the 5-line duplication).
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// digestItemFullSelect — список колонок GetDigestItem: digestItemSelect +
// digest_id (M:-view строит «◀ Назад» S:<did>:<sid> по самой строке, план
// §4.6; List*-методы digest_id не читают — он известен из запроса).
const digestItemFullSelect = `SELECT digest_id, id, COALESCE(source_id, 0), dedup_key, title, url, tg_peer, tg_msg_id, published_at, snippet, summary, full_text, class, media_kind, media_ref
	 FROM digest_items`

// GetDigestItem читает ОДНУ строку digest_items по id (Stage 9D, M:-view).
// Неизвестный/crafted id → ok=false, НЕ ошибка (GetDigest-паттерн: crafted
// callback id не должен ронять viewer — диспетчер ack'ит «Не получилось
// открыть»). Заполняет DigestID (единственный метод, который это делает).
func (s *SQLiteStore) GetDigestItem(ctx context.Context, itemID int64) (DigestItem, bool, error) {
	row := s.db.QueryRowContext(ctx, digestItemFullSelect+" WHERE id=?", itemID)
	var it DigestItem
	err := row.Scan(&it.DigestID, &it.ID, &it.SourceID, &it.DedupKey, &it.Title, &it.URL,
		&it.TgPeer, &it.TgMsgID, &it.PublishedAt, &it.Snippet, &it.Summary,
		&it.FullText, &it.Class, &it.MediaKind, &it.MediaRef)
	if errors.Is(err, sql.ErrNoRows) {
		return DigestItem{}, false, nil
	}
	if err != nil {
		return DigestItem{}, false, fmt.Errorf("scan digest_item %d: %w", itemID, err)
	}
	return it, true, nil
}

// UpsertRunSummary пишет общее саммари выпуска в runs (таблица миграции v5;
// вызов D → runs.summary, план §4.7/§4.8.1 шаг 6). Идемпотентен по run_id:
// повторный upsert того же прогона перезаписывает summary (created_at первой
// записи сохраняется). run_id ≤ 0 → error: 0 — legacy-сентинел «не прогон»
// (ListRunDigests), строки runs для него быть не должно.
func (s *SQLiteStore) UpsertRunSummary(ctx context.Context, runID int64, summary string) error {
	if runID <= 0 {
		return fmt.Errorf("upsert run summary: invalid run_id %d", runID)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO runs(run_id, summary, created_at) VALUES(?,?,?)
		 ON CONFLICT(run_id) DO UPDATE SET summary=excluded.summary`,
		runID, summary, now)
	if err != nil {
		return fmt.Errorf("upsert run %d summary: %w", runID, err)
	}
	return nil
}

// GetRunSummary читает общее саммари выпуска (восстановление TOC:<runID> —
// план 9D: «нет строки → overall-блок отсутствует — legacy-прогоны»). Нет
// строки → ("", false, nil), НЕ ошибка (GetDigest-паттерн). runID=0 →
// legacy «не прогон», всегда miss (ListRunDigests-паттерн).
func (s *SQLiteStore) GetRunSummary(ctx context.Context, runID int64) (string, bool, error) {
	if runID == 0 {
		return "", false, nil
	}
	var summary string
	err := s.db.QueryRowContext(ctx, "SELECT summary FROM runs WHERE run_id=?", runID).Scan(&summary)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("select run %d summary: %w", runID, err)
	}
	return summary, true, nil
}
