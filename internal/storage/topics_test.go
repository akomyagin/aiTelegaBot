package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// makeV2DB builds a SQLite file at schema version 2 (pre-topics) with one
// legacy source row, so migration v3 can be tested as a real upgrade.
func makeV2DB(t *testing.T, path string) {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer func() { _ = db.Close() }()
	for i := 0; i < 2; i++ {
		if _, err := db.ExecContext(ctx, migrations[i]); err != nil {
			t.Fatalf("apply migration %d: %v", i+1, err)
		}
	}
	if _, err := db.ExecContext(ctx, "PRAGMA user_version = 2"); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO sources(kind, ref, enabled, added_at) VALUES('rss','http://old.example',1,'2026-01-01T00:00:00Z')"); err != nil {
		t.Fatalf("insert legacy source: %v", err)
	}
}

// TestMigrate_V3OnTopOfV2: opening a v2 database applies migration v3
// (user_version=3), legacy sources get origin='dynamic' and topic_id NULL
// (read back as 1 via COALESCE), and the "Без темы" seed row exists.
func TestMigrate_V3OnTopOfV2(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v2.db")
	ctx := context.Background()
	makeV2DB(t, path)

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open over v2 db: %v", err)
	}

	var version int
	if err := st.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != 3 {
		t.Errorf("user_version = %d, want 3", version)
	}

	// Raw check: legacy row keeps its data, origin defaults, topic NULL.
	var origin string
	var topicID sql.NullInt64
	if err := st.db.QueryRowContext(ctx,
		"SELECT origin, topic_id FROM sources WHERE ref='http://old.example'").Scan(&origin, &topicID); err != nil {
		t.Fatalf("select legacy source: %v", err)
	}
	if origin != "dynamic" {
		t.Errorf("legacy origin = %q, want %q", origin, "dynamic")
	}
	if topicID.Valid {
		t.Errorf("legacy topic_id = %v, want NULL", topicID.Int64)
	}

	// Seeded default topic.
	var name string
	if err := st.db.QueryRowContext(ctx, "SELECT name FROM topics WHERE id=1").Scan(&name); err != nil {
		t.Fatalf("select seeded topic: %v", err)
	}
	if name != "Без темы" {
		t.Errorf("topic 1 name = %q, want %q", name, "Без темы")
	}

	// ListSources reads origin and COALESCE(topic_id, 1).
	srcs, err := st.ListSources(ctx)
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	if len(srcs) != 1 {
		t.Fatalf("ListSources returned %d rows, want 1", len(srcs))
	}
	if srcs[0].Origin != "dynamic" || srcs[0].TopicID != 1 {
		t.Errorf("legacy source = origin %q topicID %d, want dynamic/1", srcs[0].Origin, srcs[0].TopicID)
	}

	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen: v3 must not re-apply or fail.
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = st2.Close() }()
	if err := st2.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read user_version after reopen: %v", err)
	}
	if version != 3 {
		t.Errorf("user_version after reopen = %d, want 3", version)
	}
}

// TestCreateTopic_Idempotent: two calls with the same name return the same id
// and leave exactly one row.
func TestCreateTopic_Idempotent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	id1, err := st.CreateTopic(ctx, "Новости")
	if err != nil {
		t.Fatalf("first CreateTopic: %v", err)
	}
	id2, err := st.CreateTopic(ctx, "Новости")
	if err != nil {
		t.Fatalf("second CreateTopic: %v", err)
	}
	if id1 != id2 {
		t.Errorf("ids differ: %d vs %d", id1, id2)
	}
	if id1 == 1 {
		t.Errorf("new topic got id 1 — the seed row must own it")
	}

	topics, err := st.ListTopics(ctx)
	if err != nil {
		t.Fatalf("ListTopics: %v", err)
	}
	count := 0
	for _, tp := range topics {
		if tp.Name == "Новости" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("rows named Новости = %d, want 1", count)
	}
}

// TestSetSourceTopic: found for an existing source, not-found for an unknown
// id; the binding is visible through ListSources and ListTopics.
func TestSetSourceTopic(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	topicID, err := st.CreateTopic(ctx, "AI")
	if err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	srcID, err := st.AddSource(ctx, "rss", "http://a")
	if err != nil {
		t.Fatalf("AddSource: %v", err)
	}

	tests := []struct {
		name      string
		sourceID  int64
		topicID   int64
		wantFound bool
	}{
		{"existing source", srcID, topicID, true},
		{"unknown source", 9999, topicID, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			found, err := st.SetSourceTopic(ctx, tt.sourceID, tt.topicID)
			if err != nil {
				t.Fatalf("SetSourceTopic: %v", err)
			}
			if found != tt.wantFound {
				t.Errorf("found = %v, want %v", found, tt.wantFound)
			}
		})
	}

	srcs, err := st.ListSources(ctx)
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	if len(srcs) != 1 || srcs[0].TopicID != topicID {
		t.Errorf("source topic = %+v, want TopicID %d", srcs, topicID)
	}
}

// TestUpsertStaticSource_KeepsTopicID: a repeated startup upsert re-enables the
// row and keeps origin='static', but must NOT reset a user-set topic_id.
func TestUpsertStaticSource_KeepsTopicID(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if err := st.UpsertStaticSource(ctx, "arxiv", "frontpage"); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	srcs, err := st.ListSources(ctx)
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	if len(srcs) != 1 {
		t.Fatalf("rows = %d, want 1", len(srcs))
	}
	if srcs[0].Origin != "static" || !srcs[0].Enabled || srcs[0].TopicID != 1 {
		t.Errorf("after first upsert = %+v, want static/enabled/topic 1", srcs[0])
	}

	topicID, err := st.CreateTopic(ctx, "Наука")
	if err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	if found, err := st.SetSourceTopic(ctx, srcs[0].ID, topicID); err != nil || !found {
		t.Fatalf("SetSourceTopic: found=%v err=%v", found, err)
	}
	if found, err := st.DisableSource(ctx, srcs[0].ID); err != nil || !found {
		t.Fatalf("DisableSource: found=%v err=%v", found, err)
	}

	// Second startup: re-enable, keep topic.
	if err := st.UpsertStaticSource(ctx, "arxiv", "frontpage"); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	srcs2, err := st.ListSources(ctx)
	if err != nil {
		t.Fatalf("ListSources after second upsert: %v", err)
	}
	if len(srcs2) != 1 {
		t.Fatalf("rows after second upsert = %d, want 1", len(srcs2))
	}
	if srcs2[0].Origin != "static" || !srcs2[0].Enabled || srcs2[0].TopicID != topicID {
		t.Errorf("after second upsert = %+v, want static/enabled/topic %d", srcs2[0], topicID)
	}
}

// TestListTopics_SourceCount: NULL topic_id counts toward "Без темы"; bound
// sources count toward their topic; disabled sources are counted too (same
// semantics as ListSources, documented on ListTopics).
func TestListTopics_SourceCount(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	aiID, err := st.CreateTopic(ctx, "AI")
	if err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}

	s1, err := st.AddSource(ctx, "rss", "http://a") // NULL topic → "Без темы"
	if err != nil {
		t.Fatalf("AddSource s1: %v", err)
	}
	s2, err := st.AddSource(ctx, "rss", "http://b") // → AI
	if err != nil {
		t.Fatalf("AddSource s2: %v", err)
	}
	s3, err := st.AddSource(ctx, "rss", "http://c") // → AI, then disabled
	if err != nil {
		t.Fatalf("AddSource s3: %v", err)
	}
	_ = s1
	for _, id := range []int64{s2, s3} {
		if found, err := st.SetSourceTopic(ctx, id, aiID); err != nil || !found {
			t.Fatalf("SetSourceTopic %d: found=%v err=%v", id, found, err)
		}
	}
	if _, err := st.DisableSource(ctx, s3); err != nil {
		t.Fatalf("DisableSource: %v", err)
	}

	topics, err := st.ListTopics(ctx)
	if err != nil {
		t.Fatalf("ListTopics: %v", err)
	}
	want := []Topic{{ID: 1, Name: "Без темы", SourceCount: 1}, {ID: aiID, Name: "AI", SourceCount: 2}}
	if len(topics) != len(want) {
		t.Fatalf("topics = %+v, want %+v", topics, want)
	}
	for i := range want {
		if topics[i] != want[i] {
			t.Errorf("topics[%d] = %+v, want %+v", i, topics[i], want[i])
		}
	}
}

// TestPruneStaticSources (ревью M4): static-строки, отсутствующие в keep,
// удаляются; keep-строки и dynamic-строки не трогаются; повторный прогон
// идемпотентен (removed=0).
func TestPruneStaticSources(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if err := st.UpsertStaticSource(ctx, "rss", "https://keep.example/f.xml"); err != nil {
		t.Fatalf("UpsertStaticSource keep: %v", err)
	}
	if err := st.UpsertStaticSource(ctx, "hn", "frontpage"); err != nil {
		t.Fatalf("UpsertStaticSource orphan: %v", err)
	}
	dynID, err := st.AddSource(ctx, "tg_public", "dynamic_chan")
	if err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	// Осиротевшая строка была привязана к теме — удаляется вместе со строкой.
	orphanTopicID, err := st.CreateTopic(ctx, "OrphanTopic")
	if err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	var orphanRowID int64
	pre, err := st.ListSources(ctx)
	if err != nil {
		t.Fatalf("ListSources (pre): %v", err)
	}
	for _, r := range pre {
		if r.Origin == "static" && r.Kind == "hn" {
			orphanRowID = r.ID
		}
	}
	if orphanRowID == 0 {
		t.Fatalf("orphan static row not found in %+v", pre)
	}
	if _, err := st.SetSourceTopic(ctx, orphanRowID, orphanTopicID); err != nil {
		t.Fatalf("SetSourceTopic: %v", err)
	}

	keep := []StaticRef{{Kind: "rss", Ref: "https://keep.example/f.xml"}}
	removed, err := st.PruneStaticSources(ctx, keep)
	if err != nil {
		t.Fatalf("PruneStaticSources: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}

	rows, err := st.ListSources(ctx)
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2 (keep + dynamic)", rows)
	}
	for _, r := range rows {
		switch {
		case r.Origin == "static":
			if r.Kind != "rss" || r.Ref != "https://keep.example/f.xml" {
				t.Errorf("static row = %+v, want the kept rss row", r)
			}
		case r.ID == dynID && r.Kind == "tg_public" && r.Origin == "dynamic":
			// dynamic-строка не тронута
		default:
			t.Errorf("unexpected row %+v", r)
		}
	}

	// Повторный прогон с тем же keep — удалять нечего.
	removed2, err := st.PruneStaticSources(ctx, keep)
	if err != nil {
		t.Fatalf("PruneStaticSources (2): %v", err)
	}
	if removed2 != 0 {
		t.Errorf("removed2 = %d, want 0 (идемпотентность)", removed2)
	}
}
