// Pipeline tests of Stage 8 C (план §C, тест-кейс 3): real temp SQLite +
// fakes (Poster records calls, TopicSummarizer is a recording fake, Now is
// fixed). Семантика частичного сбоя — шаги 5–8 модели доставки.

package digest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
	"github.com/akomyagin/aiTelegaBot/internal/llm"
	"github.com/akomyagin/aiTelegaBot/internal/storage"
	"github.com/akomyagin/aiTelegaBot/internal/telegram"
)

var testNow = time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)

// --- fakes ---

type fakeSource struct {
	name  string
	items []feed.Item
	err   error
}

func (f fakeSource) Name() string { return f.name }
func (f fakeSource) Collect(ctx context.Context) ([]feed.Item, error) {
	return f.items, f.err
}

type fakePoster struct {
	mu      sync.Mutex
	chatIDs []string
	texts   []string
	buttons [][][]telegram.InlineButton
	err     error
}

func (p *fakePoster) DeliverWithButtons(ctx context.Context, chatID, text string,
	buttons [][]telegram.InlineButton) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.chatIDs = append(p.chatIDs, chatID)
	p.texts = append(p.texts, text)
	p.buttons = append(p.buttons, buttons)
	return p.err
}

func (p *fakePoster) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.texts)
}

type fakeSummarizer struct {
	mu        sync.Mutex
	fail      map[string]bool // topic name → error
	emptyHead bool            // Headline="" (fallback-ветка RenderTopicBody)
	calls     []string
}

func (f *fakeSummarizer) SummarizeTopic(ctx context.Context, topicName string,
	items []feed.Item) (llm.TopicSummary, error) {
	f.mu.Lock()
	f.calls = append(f.calls, topicName)
	fail := f.fail[topicName]
	empty := f.emptyHead
	f.mu.Unlock()
	if fail {
		return llm.TopicSummary{}, fmt.Errorf("boom: %s", topicName)
	}
	ts := llm.TopicSummary{}
	if !empty {
		ts.Headline = "H:" + topicName
	}
	for i := range items {
		ts.Items = append(ts.Items, llm.ItemSummary{
			Index: i + 1, Summary: "s" + strconv.Itoa(i+1),
		})
	}
	return ts, nil
}

// failMarkStore fails MarkDelivered on demand (план §C шаг 8: log-only).
type failMarkStore struct {
	storage.Store
	mu   sync.Mutex
	fail bool
	n    int
}

func (s *failMarkStore) MarkDelivered(ctx context.Context, digestID int64,
	items []storage.DigestItem) error {
	s.mu.Lock()
	s.n++
	fail := s.fail
	s.mu.Unlock()
	if fail {
		return errors.New("mark delivered boom")
	}
	return s.Store.MarkDelivered(ctx, digestID, items)
}

// --- helpers ---

func openTestStore(t *testing.T) *storage.SQLiteStore {
	t.Helper()
	st, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func makeItems(prefix string, n int) []feed.Item {
	items := make([]feed.Item, 0, n)
	for i := 1; i <= n; i++ {
		items = append(items, feed.Item{
			URL:   fmt.Sprintf("https://a.test/%s/%d", prefix, i),
			Title: fmt.Sprintf("%s-T%d", prefix, i),
			Text:  fmt.Sprintf("%s text %d", prefix, i),
		})
	}
	return items
}

func topicName(t *testing.T, st *storage.SQLiteStore, id int64) string {
	t.Helper()
	topics, err := st.ListTopics(context.Background())
	if err != nil {
		t.Fatalf("ListTopics: %v", err)
	}
	for _, tp := range topics {
		if tp.ID == id {
			return tp.Name
		}
	}
	t.Fatalf("topic %d not found", id)
	return ""
}

// --- grouping (кейс 3: темы 1 и 5 + TopicID=0 → одно оглавление) ---

func TestPipelineRun_GroupingAndDelivery(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	for _, name := range []string{"Two", "Three", "Four", "Five"} {
		if _, err := st.CreateTopic(ctx, name); err != nil {
			t.Fatalf("CreateTopic(%s): %v", name, err)
		}
	}
	bezTemy := topicName(t, st, 1)

	sum := &fakeSummarizer{}
	poster := &fakePoster{}
	p := &Pipeline{
		SourceProvider: func(ctx context.Context) ([]TrackedSource, error) {
			return []TrackedSource{
				{Src: fakeSource{name: "b", items: makeItems("b", 1)}, SourceID: 8, TopicID: 5},
				{Src: fakeSource{name: "a", items: makeItems("a", 2)}, SourceID: 7, TopicID: 0}, // → «Без темы»
			}, nil
		},
		Store: st, TopicSummary: sum, Poster: poster,
		ChatID: "111", Log: testLogger(), Now: func() time.Time { return testNow },
	}
	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if poster.calls() != 1 {
		t.Fatalf("poster calls = %d, want exactly 1 TOC message", poster.calls())
	}
	if poster.chatIDs[0] != "111" {
		t.Errorf("chatID = %q, want 111", poster.chatIDs[0])
	}
	// Порядок тем — id asc: «Без темы» (1) раньше Five (5); пустые темы 2–4
	// (нет fresh-элементов) в оглавление не попали.
	text := poster.texts[0]
	if !strings.Contains(text, "Тем: 2") {
		t.Errorf("TOC text lacks «Тем: 2»:\n%s", text)
	}
	i1 := strings.Index(text, bezTemy)
	i5 := strings.Index(text, "Five")
	if i1 < 0 || i5 < 0 || i1 > i5 {
		t.Errorf("TOC order wrong (want %q before Five):\n%s", bezTemy, text)
	}

	// Строки digests: runID = testNow.Unix(), порядок topic_id ASC, delivered=1.
	runID := testNow.Unix()
	rows, err := st.ListRunDigests(ctx, runID)
	if err != nil {
		t.Fatalf("ListRunDigests: %v", err)
	}
	if len(rows) != 2 || rows[0].TopicID != 1 || rows[1].TopicID != 5 {
		t.Fatalf("rows = %+v, want topics [1 5]", rows)
	}
	for _, r := range rows {
		if !r.Delivered {
			t.Errorf("digest %d not delivered", r.ID)
		}
	}
	// Кнопки: T:<id темы 1>, T:<id темы 5> — по ряду на тему.
	btns := poster.buttons[0]
	if len(btns) != 2 || len(btns[0]) != 1 || len(btns[1]) != 1 {
		t.Fatalf("buttons = %v, want 2 rows × 1", btns)
	}
	if want := "T:" + strconv.FormatInt(rows[0].ID, 10); btns[0][0].Data != want {
		t.Errorf("btn[0] data = %q, want %q", btns[0][0].Data, want)
	}
	if want := "T:" + strconv.FormatInt(rows[1].ID, 10); btns[1][0].Data != want {
		t.Errorf("btn[1] data = %q, want %q", btns[1][0].Data, want)
	}

	// Все элементы seen; digest_items темы 1 — порядок collect, summary по
	// Index, SourceID из stamped-элемента.
	fresh, err := st.FilterUnseen(ctx, append(makeItems("a", 2), makeItems("b", 1)...))
	if err != nil {
		t.Fatalf("FilterUnseen: %v", err)
	}
	if len(fresh) != 0 {
		t.Errorf("want all items seen, got %d fresh", len(fresh))
	}
	ditems, err := st.ListDigestItems(ctx, rows[0].ID)
	if err != nil {
		t.Fatalf("ListDigestItems: %v", err)
	}
	if len(ditems) != 2 || ditems[0].Title != "a-T1" || ditems[1].Title != "a-T2" {
		t.Fatalf("digest items = %+v, want a-T1,a-T2 in collect order", ditems)
	}
	if ditems[0].Summary != "s1" || ditems[1].Summary != "s2" {
		t.Errorf("summaries = (%q,%q), want (s1,s2)", ditems[0].Summary, ditems[1].Summary)
	}
	if ditems[0].SourceID != 7 {
		t.Errorf("digest item SourceID = %d, want 7 (taggedSource stamp)", ditems[0].SourceID)
	}
	// Body темы сохранён (RenderTopicBody): имя + headline + count.
	if !strings.Contains(rows[0].Body, "H:"+bezTemy) ||
		!strings.Contains(rows[0].Body, "— 2 материалов") {
		t.Errorf("stored body unexpected:\n%s", rows[0].Body)
	}
}

// Fallback-ветка RenderTopicBody: пустой Headline → «• <title>» первых
// элементов (отклонение C1 №2 — заголовки передаёт Run).
func TestPipelineRun_EmptyHeadlineFallbackTitles(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	sum := &fakeSummarizer{emptyHead: true}
	poster := &fakePoster{}
	p := &Pipeline{
		SourceProvider: func(ctx context.Context) ([]TrackedSource, error) {
			return []TrackedSource{{Src: fakeSource{name: "a", items: makeItems("a", 2)}, TopicID: 1}}, nil
		},
		Store: st, TopicSummary: sum, Poster: poster,
		Log: testLogger(), Now: func() time.Time { return testNow },
	}
	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows, _ := st.ListRunDigests(ctx, testNow.Unix())
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if !strings.Contains(rows[0].Body, "• a-T1") || !strings.Contains(rows[0].Body, "• a-T2") {
		t.Errorf("fallback title bullets missing in body:\n%s", rows[0].Body)
	}
}

// --- taggedSource: stamp на копии, частичная ошибка, -race параллелизм ---

func TestTaggedSource_StampCopyAndPartialError(t *testing.T) {
	orig := makeItems("x", 2)
	inner := fakeSource{name: "s", items: orig, err: errors.New("partial fail")}
	ts := taggedSource{inner: inner, sourceID: 3, topicID: 5}

	got, err := ts.Collect(context.Background())
	if err == nil || err.Error() != "partial fail" {
		t.Errorf("err = %v, want the inner error as-is", err)
	}
	if len(got) != 2 {
		t.Fatalf("items = %d, want 2 (partial items returned with the error)", len(got))
	}
	for _, it := range got {
		if it.SourceID != 3 || it.TopicID != 5 {
			t.Errorf("stamp missing: %+v", it)
		}
	}
	for _, it := range orig {
		if it.SourceID != 0 || it.TopicID != 0 {
			t.Fatalf("original items mutated: %+v", it)
		}
	}
	if ts.Name() != "s" {
		t.Errorf("Name = %q, want s", ts.Name())
	}

	// -race: Collect нескольких taggedSource одним feed.Collect (последоват-
	// ельный обход; -race страхует будущую конкуренцию и stamping-копии).
	srcs := make([]taggedSource, 4)
	for i := range srcs {
		srcs[i] = taggedSource{
			inner:    fakeSource{name: fmt.Sprintf("p%d", i), items: makeItems(fmt.Sprint(i), 5)},
			sourceID: int64(i + 1), topicID: int64(i + 1),
		}
	}
	var wg sync.WaitGroup
	for i := range srcs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			items, _ := srcs[i].Collect(context.Background())
			if len(items) != 5 {
				t.Errorf("source %d: %d items", i, len(items))
			}
		}(i)
	}
	wg.Wait()
}

// --- нет fresh: тихо, без оглавления и строк ---

func TestPipelineRun_NoFreshItems(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	poster := &fakePoster{}
	src := fakeSource{name: "a", items: makeItems("a", 2)}
	newP := func(now time.Time) *Pipeline {
		return &Pipeline{
			SourceProvider: func(ctx context.Context) ([]TrackedSource, error) {
				return []TrackedSource{{Src: src, TopicID: 1}}, nil
			},
			Store: st, TopicSummary: &fakeSummarizer{}, Poster: poster,
			Log: testLogger(), Now: func() time.Time { return now },
		}
	}
	if err := newP(testNow).Run(ctx); err != nil {
		t.Fatalf("Run(1): %v", err)
	}
	if poster.calls() != 1 {
		t.Fatalf("first run deliveries = %d, want 1", poster.calls())
	}
	// «Рестарт»: второй прогон теми же элементами, другой now.
	if err := newP(testNow.Add(time.Hour)).Run(ctx); err != nil {
		t.Fatalf("Run(2): %v", err)
	}
	if poster.calls() != 1 {
		t.Errorf("second run delivered again: calls = %d, want 1", poster.calls())
	}
	rows, err := st.ListRunDigests(ctx, testNow.Add(time.Hour).Unix())
	if err != nil {
		t.Fatalf("ListRunDigests: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("no-fresh run created %d digests rows, want 0", len(rows))
	}
}

// --- частичный сбой: тема пропущена, её элементы НЕ seen ---

func TestPipelineRun_PartialTopicFailure(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	fiveID, err := st.CreateTopic(ctx, "Five")
	if err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	sum := &fakeSummarizer{fail: map[string]bool{"Five": true}}
	poster := &fakePoster{}
	bItems := makeItems("b", 1)
	aItems := makeItems("a", 2)
	p := &Pipeline{
		SourceProvider: func(ctx context.Context) ([]TrackedSource, error) {
			return []TrackedSource{
				{Src: fakeSource{name: "b", items: bItems}, TopicID: fiveID},
				{Src: fakeSource{name: "a", items: aItems}, TopicID: 1},
			}, nil
		},
		Store: st, TopicSummary: sum, Poster: poster,
		Log: testLogger(), Now: func() time.Time { return testNow },
	}
	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run = %v, want nil (partial failure is not fatal)", err)
	}
	if poster.calls() != 1 || strings.Contains(poster.texts[0], "Five") {
		t.Fatalf("TOC must contain only the surviving topic:\n%s", poster.texts[0])
	}
	if !strings.Contains(poster.texts[0], "Тем: 1") {
		t.Errorf("TOC lacks «Тем: 1»:\n%s", poster.texts[0])
	}
	// Строки темы Five нет; единственная строка — тема 1.
	rows, _ := st.ListRunDigests(ctx, testNow.Unix())
	if len(rows) != 1 || rows[0].TopicID != 1 {
		t.Fatalf("rows = %+v, want a single topic-1 row", rows)
	}
	// Элементы упавшей темы НЕ seen — следующий прогон их подбирает.
	fresh, err := st.FilterUnseen(ctx, append(append([]feed.Item{}, bItems...), aItems...))
	if err != nil {
		t.Fatalf("FilterUnseen: %v", err)
	}
	if len(fresh) != 1 || fresh[0].Title != "b-T1" {
		t.Errorf("fresh = %+v, want only b-T1 (failed topic's items unseen)", fresh)
	}
}

// --- все темы упали → ошибка Run ---

func TestPipelineRun_AllTopicsFail(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	bezTemy := topicName(t, st, 1)
	sum := &fakeSummarizer{fail: map[string]bool{bezTemy: true, "Five": true}}
	if _, err := st.CreateTopic(ctx, "Five"); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	poster := &fakePoster{}
	items := makeItems("a", 2)
	p := &Pipeline{
		SourceProvider: func(ctx context.Context) ([]TrackedSource, error) {
			return []TrackedSource{{Src: fakeSource{name: "a", items: items}, TopicID: 1}}, nil
		},
		Store: st, TopicSummary: sum, Poster: poster,
		Log: testLogger(), Now: func() time.Time { return testNow },
	}
	err := p.Run(ctx)
	if err == nil {
		t.Fatal("Run = nil, want error when every topic failed")
	}
	if poster.calls() != 0 {
		t.Errorf("poster called %d times, want 0", poster.calls())
	}
	fresh, _ := st.FilterUnseen(ctx, items)
	if len(fresh) != 2 {
		t.Errorf("items must stay unseen, got %d fresh", len(fresh))
	}
}

// --- ошибка доставки → ошибка Run, всё откатывается на повтор ---

func TestPipelineRun_DeliveryFailure(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	poster := &fakePoster{err: errors.New("network down")}
	items := makeItems("a", 2)
	p := &Pipeline{
		SourceProvider: func(ctx context.Context) ([]TrackedSource, error) {
			return []TrackedSource{{Src: fakeSource{name: "a", items: items}, TopicID: 1}}, nil
		},
		Store: st, TopicSummary: &fakeSummarizer{}, Poster: poster,
		Log: testLogger(), Now: func() time.Time { return testNow },
	}
	err := p.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "deliver toc") {
		t.Fatalf("Run err = %v, want «deliver toc: …»", err)
	}
	rows, _ := st.ListRunDigests(ctx, testNow.Unix())
	if len(rows) != 1 || rows[0].Delivered {
		t.Fatalf("rows = %+v, want a single delivered=0 row", rows)
	}
	ditems, _ := st.ListDigestItems(ctx, rows[0].ID)
	if len(ditems) != 0 {
		t.Errorf("digest_items = %d, want 0 (MarkDelivered not called)", len(ditems))
	}
	fresh, _ := st.FilterUnseen(ctx, items)
	if len(fresh) != 2 {
		t.Errorf("items must stay unseen, got %d fresh", len(fresh))
	}
}

// --- сбой MarkDelivered после успешной доставки → Run=nil (log-only) ---

func TestPipelineRun_MarkDeliveredFailureIsLogOnly(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	fms := &failMarkStore{Store: st, fail: true}
	poster := &fakePoster{}
	items := makeItems("a", 2)
	p := &Pipeline{
		SourceProvider: func(ctx context.Context) ([]TrackedSource, error) {
			return []TrackedSource{{Src: fakeSource{name: "a", items: items}, TopicID: 1}}, nil
		},
		Store: fms, TopicSummary: &fakeSummarizer{}, Poster: poster,
		Log: testLogger(), Now: func() time.Time { return testNow },
	}
	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run = %v, want nil (post-delivery failure is log-only)", err)
	}
	if poster.calls() != 1 {
		t.Errorf("poster calls = %d, want 1", poster.calls())
	}
	if fms.n != 1 {
		t.Errorf("MarkDelivered calls = %d, want 1", fms.n)
	}
	rows, _ := st.ListRunDigests(ctx, testNow.Unix())
	if len(rows) != 1 || rows[0].Delivered {
		t.Errorf("rows = %+v, want delivered=0 after the failed mark", rows)
	}
	fresh, _ := st.FilterUnseen(ctx, items)
	if len(fresh) != 2 {
		t.Errorf("items stay unseen (duplicate into the next run), got %d fresh", len(fresh))
	}
}

// --- offline-суммаризатор в pipeline: детерминированный headline ---

func TestPipelineRun_OfflineSummarizerDeterministic(t *testing.T) {
	ctx := context.Background()
	bodies := make([]string, 0, 2)
	for run := 0; run < 2; run++ {
		st := openTestStore(t)
		poster := &fakePoster{}
		p := &Pipeline{
			SourceProvider: func(ctx context.Context) ([]TrackedSource, error) {
				return []TrackedSource{{Src: fakeSource{name: "a", items: makeItems("a", 3)}, TopicID: 1}}, nil
			},
			Store: st, TopicSummary: llm.NewOffline(), Poster: poster,
			Log: testLogger(), Now: func() time.Time { return testNow },
		}
		if err := p.Run(ctx); err != nil {
			t.Fatalf("Run(%d): %v", run, err)
		}
		rows, _ := st.ListRunDigests(ctx, testNow.Unix())
		if len(rows) != 1 {
			t.Fatalf("run %d: rows = %d, want 1", run, len(rows))
		}
		bodies = append(bodies, rows[0].Body)
	}
	if bodies[0] != bodies[1] {
		t.Errorf("offline body is not deterministic:\n%q\n%q", bodies[0], bodies[1])
	}
	if !strings.Contains(bodies[0], "- a-T1") {
		t.Errorf("offline headline lacks «- a-T1»:\n%s", bodies[0])
	}
}

// --- restart-survival (pipeline-уровень): прогон → «рестарт» → T:/TOC: из БД ---

func TestPipelineRun_RestartSurvival(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	ctx := context.Background()

	st, err := storage.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	fiveID, err := st.CreateTopic(ctx, "Five")
	if err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	poster := &fakePoster{}
	p := &Pipeline{
		SourceProvider: func(ctx context.Context) ([]TrackedSource, error) {
			return []TrackedSource{
				{Src: fakeSource{name: "b", items: makeItems("b", 1)}, SourceID: 8, TopicID: fiveID},
				{Src: fakeSource{name: "a", items: makeItems("a", 2)}, SourceID: 7, TopicID: 0},
			}, nil
		},
		Store: st, TopicSummary: &fakeSummarizer{}, Poster: poster,
		Log: testLogger(), Now: func() time.Time { return testNow },
	}
	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	runID := testNow.Unix()
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// «Рестарт»: новый Pipeline/Store поверх той же БД.
	st2, err := storage.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = st2.Close() })

	// TOC: — оглавление восстанавливается из ListRunDigests + ListTopics.
	rows, err := st2.ListRunDigests(ctx, runID)
	if err != nil {
		t.Fatalf("ListRunDigests: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows after restart = %d, want 2", len(rows))
	}
	topics, err := st2.ListTopics(ctx)
	if err != nil {
		t.Fatalf("ListTopics: %v", err)
	}
	names := map[int64]string{}
	for _, tp := range topics {
		names[tp.ID] = tp.Name
	}
	entries := make([]TOCEntry, 0, len(rows))
	for _, r := range rows {
		entries = append(entries, TOCEntry{DigestID: r.ID, TopicName: names[r.TopicID], ItemCount: r.ItemCount})
	}
	toc := RenderTOC(entries, "20 Oct 2026")
	if !strings.Contains(toc, "Тем: 2") || !strings.Contains(toc, "Five") {
		t.Errorf("restored TOC unexpected:\n%s", toc)
	}
	// T: — L1 темы восстанавливается из GetDigest + ListDigestItems.
	d, ok, err := st2.GetDigest(ctx, rows[1].ID)
	if err != nil || !ok {
		t.Fatalf("GetDigest: ok=%v err=%v", ok, err)
	}
	ditems, err := st2.ListDigestItems(ctx, rows[1].ID)
	if err != nil {
		t.Fatalf("ListDigestItems: %v", err)
	}
	l1 := RenderTopicL1(d.Body, ditems)
	if !strings.Contains(l1, d.Body) || !strings.Contains(l1, "b-T1") {
		t.Errorf("restored L1 unexpected:\n%s", l1)
	}
	// Повторный прогон после рестарта: всё seen, доставки нет.
	poster2 := &fakePoster{}
	p2 := &Pipeline{
		SourceProvider: p.SourceProvider, Store: st2,
		TopicSummary: &fakeSummarizer{}, Poster: poster2,
		Log: testLogger(), Now: func() time.Time { return testNow.Add(time.Hour) },
	}
	if err := p2.Run(ctx); err != nil {
		t.Fatalf("Run after restart: %v", err)
	}
	if poster2.calls() != 0 {
		t.Errorf("post-restart run delivered again: calls = %d", poster2.calls())
	}
}
