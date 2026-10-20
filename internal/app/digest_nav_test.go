// Stage 8 C tests (план §C, тест-кейс 7): newDigestViewer (topic/TOC/miss/
// легаси run_id=0/битый CreatedAt) и newSourceProvider (одна выборка
// ListSources, SourceID/TopicID статиков из строк, динамики из своих строк).

package app

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
	"github.com/akomyagin/aiTelegaBot/internal/storage"
	"github.com/akomyagin/aiTelegaBot/internal/telegram"
)

// --- newDigestViewer ---

func TestNewDigestViewer_Topic(t *testing.T) {
	st := newForwardTestStore(t)
	ctx := context.Background()
	viewer := newDigestViewer(st)

	// RunID != 0 → кнопка «◀ К оглавлению» (TOC:<runID>).
	id, err := st.InsertDigest(ctx, 777, 1, "STORED BODY", 2)
	if err != nil {
		t.Fatalf("InsertDigest: %v", err)
	}
	if err := st.MarkDelivered(ctx, id, []storage.DigestItem{
		{DedupKey: "k1", Title: "Item One"},
		{DedupKey: "k2", Title: "Item Two"},
	}); err != nil {
		t.Fatalf("MarkDelivered: %v", err)
	}
	view, ok, err := viewer(ctx, telegram.DigestViewRequest{Kind: telegram.ViewKindTopic, ID: id})
	if err != nil || !ok {
		t.Fatalf("viewer(topic) = ok:%v err:%v", ok, err)
	}
	if !strings.Contains(view.Text, "STORED BODY") ||
		!strings.Contains(view.Text, "Item One") || !strings.Contains(view.Text, "Item Two") {
		t.Errorf("L1 text unexpected:\n%s", view.Text)
	}
	if len(view.Buttons) != 1 || len(view.Buttons[0]) != 1 ||
		view.Buttons[0][0].Text != "◀ К оглавлению" || view.Buttons[0][0].Data != "TOC:777" {
		t.Errorf("buttons = %+v, want one «◀ К оглавлению» row → TOC:777", view.Buttons)
	}

	// Легаси run_id=0 → оглавления нет → кнопки нет.
	legacyID, err := st.InsertDigest(ctx, 0, 1, "LEGACY", 1)
	if err != nil {
		t.Fatalf("InsertDigest(legacy): %v", err)
	}
	view, ok, err = viewer(ctx, telegram.DigestViewRequest{Kind: telegram.ViewKindTopic, ID: legacyID})
	if err != nil || !ok {
		t.Fatalf("viewer(legacy) = ok:%v err:%v", ok, err)
	}
	if len(view.Buttons) != 0 {
		t.Errorf("legacy buttons = %+v, want none", view.Buttons)
	}

	// Несуществующий id → ok=false, НЕ ошибка (crafted id не роняют viewer).
	view, ok, err = viewer(ctx, telegram.DigestViewRequest{Kind: telegram.ViewKindTopic, ID: 999999})
	if err != nil || ok {
		t.Errorf("viewer(miss) = ok:%v err:%v, want false/nil", ok, err)
	}
	// Неизвестный kind → то же.
	if _, ok, err = viewer(ctx, telegram.DigestViewRequest{Kind: "bogus", ID: id}); err != nil || ok {
		t.Errorf("viewer(bogus kind) = ok:%v err:%v, want false/nil", ok, err)
	}
}

func TestNewDigestViewer_TOC(t *testing.T) {
	st := newForwardTestStore(t)
	ctx := context.Background()
	viewer := newDigestViewer(st)

	fiveID, err := st.CreateTopic(ctx, "Five")
	if err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	id1, err := st.InsertDigest(ctx, 888, 1, "B1", 2)
	if err != nil {
		t.Fatalf("InsertDigest: %v", err)
	}
	id5, err := st.InsertDigest(ctx, 888, fiveID, "B5", 1)
	if err != nil {
		t.Fatalf("InsertDigest: %v", err)
	}
	_ = id1

	view, ok, err := viewer(ctx, telegram.DigestViewRequest{Kind: telegram.ViewKindTOC, ID: 888})
	if err != nil || !ok {
		t.Fatalf("viewer(toc) = ok:%v err:%v", ok, err)
	}
	// Порядок topic_id ASC, имена из ListTopics, date из CreatedAt (реальное
	// время вставки — сегодняшняя дата в том же формате).
	if !strings.Contains(view.Text, "Тем: 2") {
		t.Errorf("TOC text lacks «Тем: 2»:\n%s", view.Text)
	}
	wantDate := time.Now().UTC().Format("02 Jan 2006")
	if !strings.Contains(view.Text, "— "+wantDate) {
		t.Errorf("TOC date %q missing:\n%s", wantDate, view.Text)
	}
	i1 := strings.Index(view.Text, "Без темы")
	i5 := strings.Index(view.Text, "Five")
	if i1 < 0 || i5 < 0 || i1 > i5 {
		t.Errorf("TOC must carry topic names in topic_id ASC order:\n%s", view.Text)
	}
	if len(view.Buttons) != 2 ||
		view.Buttons[0][0].Data != "T:"+strconv.FormatInt(id1, 10) ||
		view.Buttons[1][0].Data != "T:"+strconv.FormatInt(id5, 10) {
		t.Errorf("TOC buttons = %+v, want T:%d, T:%d", view.Buttons, id1, id5)
	}

	// runID=0 → «не прогон» → not found; неизвестный runID → not found.
	if _, ok, err = viewer(ctx, telegram.DigestViewRequest{Kind: telegram.ViewKindTOC, ID: 0}); err != nil || ok {
		t.Errorf("viewer(TOC:0) = ok:%v err:%v, want false/nil", ok, err)
	}
	if _, ok, err = viewer(ctx, telegram.DigestViewRequest{Kind: telegram.ViewKindTOC, ID: 12345}); err != nil || ok {
		t.Errorf("viewer(TOC:unknown) = ok:%v err:%v, want false/nil", ok, err)
	}
}

// Битый CreatedAt первой строки → date="" (решение планировщика).
func TestNewDigestViewer_TOCBrokenCreatedAt(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "nav.db")
	st, err := storage.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := st.InsertDigest(ctx, 999, 1, "B", 1); err != nil {
		t.Fatalf("InsertDigest: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := db.ExecContext(ctx, "UPDATE digests SET created_at='garbage'"); err != nil {
		t.Fatalf("UPDATE: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("db.Close: %v", err)
	}
	st2, err := storage.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = st2.Close() })

	view, ok, err := newDigestViewer(st2)(ctx, telegram.DigestViewRequest{Kind: telegram.ViewKindTOC, ID: 999})
	if err != nil || !ok {
		t.Fatalf("viewer = ok:%v err:%v", ok, err)
	}
	// Заголовок без «— <date>» (битый CreatedAt → date=""); строки тем сами
	// содержат «—», поэтому проверяется только первая строка.
	firstLine := view.Text
	if i := strings.Index(firstLine, "\n"); i >= 0 {
		firstLine = firstLine[:i]
	}
	if firstLine != "📋 Дайджест" {
		t.Errorf("broken CreatedAt must render date=\"\", got header %q", firstLine)
	}
}

// --- newSourceProvider ---

type navFakeSource struct{ name string }

func (n navFakeSource) Name() string                                     { return n.name }
func (n navFakeSource) Collect(ctx context.Context) ([]feed.Item, error) { return nil, nil }

type countingLister struct {
	rows  []storage.Source
	err   error
	calls int
}

func (c *countingLister) ListSources(ctx context.Context) ([]storage.Source, error) {
	c.calls++
	return c.rows, c.err
}

func TestNewSourceProvider_TrackedSources(t *testing.T) {
	ctx := context.Background()
	lister := &countingLister{rows: []storage.Source{
		{ID: 4, Kind: "rss", Ref: "u1", Enabled: true, Origin: "static", TopicID: 3},
		{ID: 5, Kind: "rss", Ref: "d1", Enabled: true, Origin: "dynamic", TopicID: 2},
	}}
	statics := []feed.Source{navFakeSource{name: "s1"}, navFakeSource{name: "s2"}}
	refs := []staticSourceRef{{Kind: "rss", Ref: "u1"}, {Kind: "hn", Ref: "frontpage"}}
	provider := newSourceProvider(lister, statics, refs,
		http.DefaultClient, 20, &telegram.ChannelBuffer{}, nil, 20)

	all, err := provider(ctx)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	if lister.calls != 1 {
		t.Errorf("ListSources calls = %d, want exactly 1 per run", lister.calls)
	}
	// Статики (2) + динамика (1); статик без строки → нули → «Без темы».
	if len(all) != 3 {
		t.Fatalf("got %d tracked sources, want 3", len(all))
	}
	if all[0].Src.Name() != "s1" || all[0].SourceID != 4 || all[0].TopicID != 3 {
		t.Errorf("static with row = %+v, want s1/4/3", all[0])
	}
	if all[1].Src.Name() != "s2" || all[1].SourceID != 0 || all[1].TopicID != 0 {
		t.Errorf("static without row = %+v, want s2/0/0", all[1])
	}
	if all[2].SourceID != 5 || all[2].TopicID != 2 {
		t.Errorf("dynamic = %+v, want SourceID 5 / TopicID 2", all[2])
	}
}

func TestNewSourceProvider_ListError(t *testing.T) {
	lister := &countingLister{err: errors.New("db down")}
	provider := newSourceProvider(lister, nil, nil,
		http.DefaultClient, 20, &telegram.ChannelBuffer{}, nil, 20)
	if _, err := provider(context.Background()); err == nil {
		t.Error("want the ListSources error surfaced")
	}
}
