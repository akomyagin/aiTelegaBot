// TestMigrate_V5ToV6 (Этап 9D-2a, директива диспетчера): миграция v6
// (digests.sections_json) поверх настоящей v5-БД — собираем v5-схему вручную
// (migrations[0:5] + user_version=5), пишем LEGACY digests-строку старым
// списком колонок, Open докатывает v6: user_version=6, sections_json читается
// '' (деградация без саммари топиков), повторный Open идемпотентен; roundtrip
// InsertDigest(sectionsJSON) → GetDigest/ListRunDigests байт-в-байт.

package storage

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

func TestMigrate_V5ToV6(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v5.db")
	ctx := context.Background()

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw v5 db: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, err := raw.ExecContext(ctx, migrations[i]); err != nil {
			t.Fatalf("apply migration %d: %v", i+1, err)
		}
		if _, err := raw.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			t.Fatalf("set user_version %d: %v", i+1, err)
		}
	}
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO digests(id, created_at, item_count, body, delivered, topic_id, run_id)
		 VALUES(1, '2026-01-01T00:00:00Z', 1, 'body', 1, 1, 500)`); err != nil {
		t.Fatalf("insert legacy digest: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open over v5: %v", err)
	}
	var version int
	if err := st.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != 6 {
		t.Errorf("user_version = %d, want 6", version)
	}

	// LEGACY-строка: sections_json = '' (DEFAULT) → деградация без саммари.
	d, ok, err := st.GetDigest(ctx, 1)
	if err != nil || !ok {
		t.Fatalf("GetDigest(legacy) = ok:%v err:%v", ok, err)
	}
	if d.SectionsJSON != "" {
		t.Errorf("legacy SectionsJSON = %q, want ''", d.SectionsJSON)
	}

	// Roundtrip: InsertDigest c sections_json → GetDigest/ListRunDigests.
	const sections = `[{"source_id":3,"summary":"Саммари топика"}]`
	id, err := st.InsertDigest(ctx, 600, 1, "b", 0, sections)
	if err != nil {
		t.Fatalf("InsertDigest: %v", err)
	}
	d2, ok, err := st.GetDigest(ctx, id)
	if err != nil || !ok {
		t.Fatalf("GetDigest = ok:%v err:%v", ok, err)
	}
	if d2.SectionsJSON != sections {
		t.Errorf("SectionsJSON = %q, want %q", d2.SectionsJSON, sections)
	}
	rows, err := st.ListRunDigests(ctx, 600)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListRunDigests = %d rows, %v", len(rows), err)
	}
	if rows[0].SectionsJSON != sections {
		t.Errorf("ListRunDigests SectionsJSON = %q, want %q", rows[0].SectionsJSON, sections)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Повторный Open идемпотентен (v6 не применяется дважды).
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	if err := st2.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read user_version after reopen: %v", err)
	}
	if version != 6 {
		t.Errorf("user_version after reopen = %d, want 6", version)
	}
}
