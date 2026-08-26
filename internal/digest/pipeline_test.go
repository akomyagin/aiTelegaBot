package digest

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
	"github.com/akomyagin/aiTelegaBot/internal/llm"
	"github.com/akomyagin/aiTelegaBot/internal/storage"
)

// fakeSource returns fixed items.
type fakeSource struct{ items []feed.Item }

func (f *fakeSource) Collect(_ context.Context) ([]feed.Item, error) { return f.items, nil }
func (f *fakeSource) Name() string                                   { return "fake" }

// fakeDeliverer records delivered texts, or fails with err.
type fakeDeliverer struct {
	delivered []string
	err       error
}

func (f *fakeDeliverer) Deliver(_ context.Context, _, text string) error {
	if f.err != nil {
		return f.err
	}
	f.delivered = append(f.delivered, text)
	return nil
}

func newStore(t *testing.T) storage.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := storage.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func fixedNow() time.Time { return time.Date(2024, 1, 15, 9, 0, 0, 0, time.UTC) }

// staticProvider returns a SourceProvider that always yields the given sources.
func staticProvider(srcs ...feed.Source) func(context.Context) ([]feed.Source, error) {
	return func(context.Context) ([]feed.Source, error) { return srcs, nil }
}

func sampleItems() []feed.Item {
	return []feed.Item{
		{Source: "fake", Kind: "rss", Title: "One", URL: "https://ex.com/1", Text: "First body."},
		{Source: "fake", Kind: "rss", Title: "Two", URL: "https://ex.com/2", Text: "Second body."},
	}
}

func TestPipeline_FirstRunDelivers(t *testing.T) {
	store := newStore(t)
	del := &fakeDeliverer{}
	p := &Pipeline{
		SourceProvider: staticProvider(&fakeSource{items: sampleItems()}),
		Store:          store,
		Summarize:      llm.NewOffline(),
		Deliver:        del,
		ChatID:         "123",
		Now:            fixedNow,
	}

	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(del.delivered) != 1 {
		t.Fatalf("delivered %d messages, want 1", len(del.delivered))
	}
	if !strings.Contains(del.delivered[0], "2 материалов") {
		t.Errorf("body missing count: %q", del.delivered[0])
	}

	// Items now marked seen: FilterUnseen should return none.
	rest, err := store.FilterUnseen(context.Background(), sampleItems())
	if err != nil {
		t.Fatalf("FilterUnseen: %v", err)
	}
	if len(rest) != 0 {
		t.Fatalf("expected 0 unseen after run, got %d", len(rest))
	}
}

func TestPipeline_SecondRunNoDuplicate(t *testing.T) {
	store := newStore(t)
	del := &fakeDeliverer{}
	newP := func() *Pipeline {
		return &Pipeline{
			SourceProvider: staticProvider(&fakeSource{items: sampleItems()}),
			Store:          store,
			Summarize:      llm.NewOffline(),
			Deliver:        del,
			ChatID:         "123",
			Now:            fixedNow,
		}
	}

	if err := newP().Run(context.Background()); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if err := newP().Run(context.Background()); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(del.delivered) != 1 {
		t.Fatalf("delivered %d messages across two runs, want 1", len(del.delivered))
	}
}

func TestPipeline_DeliveryFailureLeavesUnseen(t *testing.T) {
	store := newStore(t)
	del := &fakeDeliverer{err: errors.New("network down")}
	p := &Pipeline{
		SourceProvider: staticProvider(&fakeSource{items: sampleItems()}),
		Store:          store,
		Summarize:      llm.NewOffline(),
		Deliver:        del,
		ChatID:         "123",
		Now:            fixedNow,
	}

	if err := p.Run(context.Background()); err == nil {
		t.Fatal("expected delivery error, got nil")
	}

	// Nothing marked seen — items remain deliverable on the next run.
	rest, err := store.FilterUnseen(context.Background(), sampleItems())
	if err != nil {
		t.Fatalf("FilterUnseen: %v", err)
	}
	if len(rest) != 2 {
		t.Fatalf("expected 2 unseen after failed delivery, got %d", len(rest))
	}
}

// TestPipeline_SourceProviderReevaluatedEachRun confirms that SourceProvider is
// called fresh on every Run — a source added between two runs (without
// recreating the Pipeline, simulating a DB-backed source added at runtime via
// /addsource) is picked up on the very next run, with no restart required.
func TestPipeline_SourceProviderReevaluatedEachRun(t *testing.T) {
	store := newStore(t)
	del := &fakeDeliverer{}

	itemA := feed.Item{Source: "fake", Kind: "rss", Title: "A", URL: "https://ex.com/a", Text: "Body A."}
	itemB := feed.Item{Source: "fake", Kind: "rss", Title: "B", URL: "https://ex.com/b", Text: "Body B."}

	var sources []feed.Source // mutated by the test between runs, like a DB snapshot
	p := &Pipeline{
		SourceProvider: func(context.Context) ([]feed.Source, error) { return sources, nil },
		Store:          store,
		Summarize:      llm.NewOffline(),
		Deliver:        del,
		ChatID:         "123",
		Now:            fixedNow,
	}

	// First run: only source A exists.
	sources = []feed.Source{&fakeSource{items: []feed.Item{itemA}}}
	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if len(del.delivered) != 1 || !strings.Contains(del.delivered[0], "1 материалов") {
		t.Fatalf("after first run: delivered=%v, want one digest with 1 item", del.delivered)
	}

	// Simulate /addsource: a second source appears WITHOUT recreating p.
	sources = []feed.Source{
		&fakeSource{items: []feed.Item{itemA}}, // still present, but already seen
		&fakeSource{items: []feed.Item{itemB}}, // newly added
	}
	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(del.delivered) != 2 {
		t.Fatalf("delivered %d digests after second run, want 2", len(del.delivered))
	}
	if !strings.Contains(del.delivered[1], "1 материалов") {
		t.Errorf("second digest should contain only the new item B, got: %q", del.delivered[1])
	}
}

func TestPipeline_EmptySourcesNoDelivery(t *testing.T) {
	store := newStore(t)
	del := &fakeDeliverer{}
	p := &Pipeline{
		SourceProvider: staticProvider(&fakeSource{items: nil}),
		Store:          store,
		Summarize:      llm.NewOffline(),
		Deliver:        del,
		ChatID:         "123",
		Now:            fixedNow,
	}

	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(del.delivered) != 0 {
		t.Fatalf("delivered %d messages, want 0", len(del.delivered))
	}
}
