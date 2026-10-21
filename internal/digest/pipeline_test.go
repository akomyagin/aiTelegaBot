// Pipeline tests of Stage 8 C (план §C) + Stage 9A (план, тест-кейс 5):
// real temp SQLite + fakes (Poster records calls, ClassifySummarizer —
// записывающий фейк со счётчиком, Now is fixed). Семантика частичного сбоя —
// §4.9: сбой темы = пропуск, сбой доставки = повтор без потери LLM-работы
// (кеш message_state).

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

// fakeOutcome — переопределение результата вызова A по одному заголовку.
type fakeOutcome struct {
	class    string // '' → other (дефолт)
	summary  string // '' → "s<индекс в батче>" (дефолт)
	degraded bool
}

// fakeClassifier — фейк вызова A (llm.ClassifySummarizer) со счётчиком
// вызовов. Дефолт: class=other, summary="s<индекс в батче>" (батч = тема,
// поэтому индексы совпадают с прежними per-topic саммари), Degraded=false.
// failPref рвёт вызов по префиксу заголовка ("a-T1" → "a") — аналог прежнего
// сбоя SummarizeTopic на тему; byTitle переопределяет результат по элементу.
type fakeClassifier struct {
	mu       sync.Mutex
	calls    int
	batches  [][]llm.ClassifyItem
	failPref map[string]bool
	byTitle  map[string]fakeOutcome
}

func titlePrefix(title string) string {
	if i := strings.IndexByte(title, '-'); i > 0 {
		return title[:i]
	}
	return title
}

func (f *fakeClassifier) ClassifySummarize(ctx context.Context,
	items []llm.ClassifyItem) (llm.ClassifyResult, error) {
	f.mu.Lock()
	f.calls++
	cp := make([]llm.ClassifyItem, len(items))
	copy(cp, items)
	f.batches = append(f.batches, cp)
	f.mu.Unlock()

	for _, it := range items {
		if f.failPref[titlePrefix(it.Title)] {
			return llm.ClassifyResult{}, fmt.Errorf("boom: %s", it.Title)
		}
	}
	res := llm.ClassifyResult{
		Items:    make([]llm.ItemClassSummary, len(items)),
		Degraded: make([]bool, len(items)),
	}
	for i, it := range items {
		out := fakeOutcome{class: llm.ClassOther, summary: "s" + strconv.Itoa(i+1)}
		if o, ok := f.byTitle[it.Title]; ok {
			if o.class != "" {
				out.class = o.class
			}
			if o.summary != "" {
				out.summary = o.summary
			}
			out.degraded = o.degraded
		}
		res.Items[i] = llm.ItemClassSummary{Index: i + 1, Class: out.class, Summary: out.summary}
		res.Degraded[i] = out.degraded
	}
	return res, nil
}

func (f *fakeClassifier) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
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

// newTestPipeline собирает Pipeline с фиксированными источниками и Now.
func newTestPipeline(st storage.Store, cls llm.ClassifySummarizer, poster MessagePoster,
	srcs []TrackedSource, now time.Time) *Pipeline {
	return &Pipeline{
		SourceProvider: func(ctx context.Context) ([]TrackedSource, error) { return srcs, nil },
		Store:          st,
		Classify:       cls,
		Poster:         poster,
		ChatID:         "111",
		Log:            testLogger(),
		Now:            func() time.Time { return now },
	}
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

func dedupKeys(items []feed.Item) []string {
	keys := make([]string, 0, len(items))
	for _, it := range items {
		keys = append(keys, it.DedupKey())
	}
	return keys
}

// --- grouping (кейс 3 Этапа 8: темы 1 и 5 + TopicID=0 → одно оглавление) ---

func TestPipelineRun_GroupingAndDelivery(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	for _, name := range []string{"Two", "Three", "Four", "Five"} {
		if _, err := st.CreateTopic(ctx, name); err != nil {
			t.Fatalf("CreateTopic(%s): %v", name, err)
		}
	}
	bezTemy := topicName(t, st, 1)

	cls := &fakeClassifier{}
	poster := &fakePoster{}
	p := newTestPipeline(st, cls, poster, []TrackedSource{
		{Src: fakeSource{name: "b", items: makeItems("b", 1)}, SourceID: 8, TopicID: 5},
		{Src: fakeSource{name: "a", items: makeItems("a", 2)}, SourceID: 7, TopicID: 0}, // → «Без темы»
	}, testNow)
	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if poster.calls() != 1 {
		t.Fatalf("poster calls = %d, want exactly 1 TOC message", poster.calls())
	}
	if poster.chatIDs[0] != "111" {
		t.Errorf("chatID = %q, want 111", poster.chatIDs[0])
	}
	if cls.callCount() != 2 {
		t.Errorf("classify calls = %d, want 2 (по одному на тему)", cls.callCount())
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
	// индексу батча, class из вызова A, SourceID из stamped-элемента.
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
	if ditems[0].Class != llm.ClassOther || ditems[1].Class != llm.ClassOther {
		t.Errorf("classes = (%q,%q), want (other,other)", ditems[0].Class, ditems[1].Class)
	}
	if ditems[0].SourceID != 7 {
		t.Errorf("digest item SourceID = %d, want 7 (taggedSource stamp)", ditems[0].SourceID)
	}
	// Body темы (9A): headline-вызова нет — fallback-ветка RenderTopicBody с
	// буллетами первых заголовков.
	if !strings.Contains(rows[0].Body, "• a-T1") ||
		!strings.Contains(rows[0].Body, "— 2 материалов") {
		t.Errorf("stored body unexpected:\n%s", rows[0].Body)
	}
}

// 9A: тело темы ВСЕГДА рендерится fallback-веткой (пустой headline — вызов C
// появится только в 9D): буллеты «• <title>» первых элементов.
func TestPipelineRun_FallbackBodyWithoutHeadline(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	cls := &fakeClassifier{}
	poster := &fakePoster{}
	p := newTestPipeline(st, cls, poster, []TrackedSource{
		{Src: fakeSource{name: "a", items: makeItems("a", 2)}, TopicID: 1},
	}, testNow)
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

// --- нет fresh: тихо, без оглавления, строк и вызовов A ---

func TestPipelineRun_NoFreshItems(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	poster := &fakePoster{}
	cls := &fakeClassifier{}
	src := fakeSource{name: "a", items: makeItems("a", 2)}
	srcs := []TrackedSource{{Src: src, TopicID: 1}}
	if err := newTestPipeline(st, cls, poster, srcs, testNow).Run(ctx); err != nil {
		t.Fatalf("Run(1): %v", err)
	}
	if poster.calls() != 1 {
		t.Fatalf("first run deliveries = %d, want 1", poster.calls())
	}
	// «Рестарт»: второй прогон теми же элементами, другой now.
	if err := newTestPipeline(st, cls, poster, srcs, testNow.Add(time.Hour)).Run(ctx); err != nil {
		t.Fatalf("Run(2): %v", err)
	}
	if poster.calls() != 1 {
		t.Errorf("second run delivered again: calls = %d, want 1", poster.calls())
	}
	if cls.callCount() != 1 {
		t.Errorf("classify calls = %d, want 1 (second run exits before LLM)", cls.callCount())
	}
	rows, err := st.ListRunDigests(ctx, testNow.Add(time.Hour).Unix())
	if err != nil {
		t.Fatalf("ListRunDigests: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("no-fresh run created %d digests rows, want 0", len(rows))
	}
}

// --- частичный сбой вызова A: тема пропущена, её элементы НЕ seen ---

func TestPipelineRun_PartialTopicFailure(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	fiveID, err := st.CreateTopic(ctx, "Five")
	if err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	cls := &fakeClassifier{failPref: map[string]bool{"b": true}}
	poster := &fakePoster{}
	bItems := makeItems("b", 1)
	aItems := makeItems("a", 2)
	p := newTestPipeline(st, cls, poster, []TrackedSource{
		{Src: fakeSource{name: "b", items: bItems}, TopicID: fiveID},
		{Src: fakeSource{name: "a", items: aItems}, TopicID: 1},
	}, testNow)
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
	// Элементы упавшей темы НЕ seen и НЕ в кеше — следующий прогон подбирает.
	fresh, err := st.FilterUnseen(ctx, append(append([]feed.Item{}, bItems...), aItems...))
	if err != nil {
		t.Fatalf("FilterUnseen: %v", err)
	}
	if len(fresh) != 1 || fresh[0].Title != "b-T1" {
		t.Errorf("fresh = %+v, want only b-T1 (failed topic's items unseen)", fresh)
	}
	states, err := st.GetMessageStates(ctx, dedupKeys(bItems))
	if err != nil {
		t.Fatalf("GetMessageStates: %v", err)
	}
	if len(states) != 0 {
		t.Errorf("failed topic cached %d states, want 0", len(states))
	}
}

// --- все темы упали → ошибка Run ---

func TestPipelineRun_AllTopicsFail(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if _, err := st.CreateTopic(ctx, "Five"); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	cls := &fakeClassifier{failPref: map[string]bool{"a": true}}
	poster := &fakePoster{}
	items := makeItems("a", 2)
	p := newTestPipeline(st, cls, poster, []TrackedSource{
		{Src: fakeSource{name: "a", items: items}, TopicID: 1},
	}, testNow)
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

// --- ошибка доставки → ошибка Run, всё откатывается на повтор; кеш вызова A
// при этом СОХРАНЕН (крах не теряет LLM-работу, §4.8.1 шаг 4a) ---

func TestPipelineRun_DeliveryFailure(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	poster := &fakePoster{err: errors.New("network down")}
	items := makeItems("a", 2)
	p := newTestPipeline(st, &fakeClassifier{}, poster, []TrackedSource{
		{Src: fakeSource{name: "a", items: items}, TopicID: 1},
	}, testNow)
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
	states, err := st.GetMessageStates(ctx, dedupKeys(items))
	if err != nil {
		t.Fatalf("GetMessageStates: %v", err)
	}
	if len(states) != 2 {
		t.Fatalf("states = %d, want 2 (LLM-работа закеширована до доставки)", len(states))
	}
	for _, s := range states {
		if s.Status != "done" {
			t.Errorf("state %s status = %q, want done", s.DedupKey, s.Status)
		}
	}
}

// --- сбой MarkDelivered после успешной доставки → Run=nil (log-only) ---

func TestPipelineRun_MarkDeliveredFailureIsLogOnly(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	fms := &failMarkStore{Store: st, fail: true}
	poster := &fakePoster{}
	items := makeItems("a", 2)
	p := newTestPipeline(fms, &fakeClassifier{}, poster, []TrackedSource{
		{Src: fakeSource{name: "a", items: items}, TopicID: 1},
	}, testNow)
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

// --- offline-классификатор в pipeline: детерминированное тело темы;
// Degraded-результат НЕ кэшируется в message_state ---

func TestPipelineRun_OfflineClassifyDeterministic(t *testing.T) {
	ctx := context.Background()
	bodies := make([]string, 0, 2)
	for run := 0; run < 2; run++ {
		st := openTestStore(t)
		poster := &fakePoster{}
		items := makeItems("a", 3)
		p := newTestPipeline(st, llm.NewOffline(), poster, []TrackedSource{
			{Src: fakeSource{name: "a", items: items}, TopicID: 1},
		}, testNow)
		if err := p.Run(ctx); err != nil {
			t.Fatalf("Run(%d): %v", run, err)
		}
		rows, _ := st.ListRunDigests(ctx, testNow.Unix())
		if len(rows) != 1 {
			t.Fatalf("run %d: rows = %d, want 1", run, len(rows))
		}
		bodies = append(bodies, rows[0].Body)
		states, err := st.GetMessageStates(ctx, dedupKeys(items))
		if err != nil {
			t.Fatalf("GetMessageStates: %v", err)
		}
		if len(states) != 0 {
			t.Errorf("run %d: offline cached %d states, want 0 (Degraded не кэшируется)", run, len(states))
		}
	}
	if bodies[0] != bodies[1] {
		t.Errorf("offline body is not deterministic:\n%q\n%q", bodies[0], bodies[1])
	}
	if !strings.Contains(bodies[0], "• a-T1") {
		t.Errorf("offline body lacks fallback bullet «• a-T1»:\n%s", bodies[0])
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
	cls := &fakeClassifier{}
	srcs := []TrackedSource{
		{Src: fakeSource{name: "b", items: makeItems("b", 1)}, SourceID: 8, TopicID: fiveID},
		{Src: fakeSource{name: "a", items: makeItems("a", 2)}, SourceID: 7, TopicID: 0},
	}
	p := newTestPipeline(st, cls, poster, srcs, testNow)
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
	// Повторный прогон после рестарта: всё seen, доставки и вызовов A нет.
	poster2 := &fakePoster{}
	p2 := newTestPipeline(st2, cls, poster2, srcs, testNow.Add(time.Hour))
	if err := p2.Run(ctx); err != nil {
		t.Fatalf("Run after restart: %v", err)
	}
	if poster2.calls() != 0 {
		t.Errorf("post-restart run delivered again: calls = %d", poster2.calls())
	}
	if cls.callCount() != 2 {
		t.Errorf("classify calls = %d, want 2 (post-restart run is fully seen)", cls.callCount())
	}
}

// --- Этап 9A, кейс 5: ad-элемент ---
// ad → нет в digest_items, сразу seen, status=dropped_ad; повторный Run с
// теми же элементами → 0 новых вызовов A (кеш + seen).

func TestPipelineRun_AdDroppedSeenAndCached(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	cls := &fakeClassifier{byTitle: map[string]fakeOutcome{
		"ad-T1": {class: llm.ClassAd},
	}}
	items := append(makeItems("ad", 1), makeItems("ok", 1)...)
	srcs := []TrackedSource{{Src: fakeSource{name: "a", items: items}, TopicID: 1}}

	poster := &fakePoster{}
	if err := newTestPipeline(st, cls, poster, srcs, testNow).Run(ctx); err != nil {
		t.Fatalf("Run(1): %v", err)
	}
	if poster.calls() != 1 {
		t.Fatalf("poster calls = %d, want 1", poster.calls())
	}
	rows, _ := st.ListRunDigests(ctx, testNow.Unix())
	if len(rows) != 1 || rows[0].ItemCount != 1 {
		t.Fatalf("rows = %+v, want one row with ItemCount=1 (ad не в счётчике)", rows)
	}
	ditems, _ := st.ListDigestItems(ctx, rows[0].ID)
	if len(ditems) != 1 || ditems[0].Title != "ok-T1" {
		t.Fatalf("digest_items = %+v, want only ok-T1", ditems)
	}
	// Оба элемента seen: ok — через MarkDelivered, ad — через MarkSeenBatch.
	fresh, _ := st.FilterUnseen(ctx, items)
	if len(fresh) != 0 {
		t.Errorf("fresh = %+v, want empty (ad seen сразу)", fresh)
	}
	states, err := st.GetMessageStates(ctx, dedupKeys(items))
	if err != nil {
		t.Fatalf("GetMessageStates: %v", err)
	}
	if s := states[items[0].DedupKey()]; s.Status != "dropped_ad" || s.Class != llm.ClassAd {
		t.Errorf("ad state = %+v, want status=dropped_ad class=ad", s)
	}
	if s := states[items[1].DedupKey()]; s.Status != "done" || s.Class != llm.ClassOther || s.Summary != "s2" {
		t.Errorf("ok state = %+v, want status=done class=other summary=s2", s)
	}
	// Повторный Run с теми же элементами: всё seen → 0 новых вызовов A.
	poster2 := &fakePoster{}
	if err := newTestPipeline(st, cls, poster2, srcs, testNow.Add(time.Hour)).Run(ctx); err != nil {
		t.Fatalf("Run(2): %v", err)
	}
	if cls.callCount() != 1 {
		t.Errorf("classify calls = %d, want 1 (повторный Run ничего не переспрашивает)", cls.callCount())
	}
	if poster2.calls() != 0 {
		t.Errorf("second run delivered again: calls = %d, want 0", poster2.calls())
	}
}

// --- Этап 9A: тема целиком из рекламы → тихий выход, не ошибка Run ---

func TestPipelineRun_AllAdsNoDigest(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	cls := &fakeClassifier{byTitle: map[string]fakeOutcome{
		"ad-T1": {class: llm.ClassAd},
		"ad-T2": {class: llm.ClassAd},
	}}
	items := makeItems("ad", 2)
	poster := &fakePoster{}
	err := newTestPipeline(st, cls, poster, []TrackedSource{
		{Src: fakeSource{name: "a", items: items}, TopicID: 1},
	}, testNow).Run(ctx)
	if err != nil {
		t.Fatalf("Run = %v, want nil (all-ads is a quiet exit, not a failure)", err)
	}
	if poster.calls() != 0 {
		t.Errorf("poster calls = %d, want 0 (доставлять нечего)", poster.calls())
	}
	rows, _ := st.ListRunDigests(ctx, testNow.Unix())
	if len(rows) != 0 {
		t.Errorf("rows = %+v, want 0", rows)
	}
	fresh, _ := st.FilterUnseen(ctx, items)
	if len(fresh) != 0 {
		t.Errorf("fresh = %+v, want empty (ads seen)", fresh)
	}
	states, _ := st.GetMessageStates(ctx, dedupKeys(items))
	for _, s := range states {
		if s.Status != "dropped_ad" {
			t.Errorf("state %s = %+v, want dropped_ad", s.DedupKey, s)
		}
	}
}

// --- Этап 9A, кейс 5: course → одна фраза, class=course в digest_items ---

func TestPipelineRun_CourseOnePhrase(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	const courseSummary = "Курс: Go — основы"
	cls := &fakeClassifier{byTitle: map[string]fakeOutcome{
		"c-T1": {class: llm.ClassCourse, summary: courseSummary},
	}}
	items := makeItems("c", 2)
	poster := &fakePoster{}
	if err := newTestPipeline(st, cls, poster, []TrackedSource{
		{Src: fakeSource{name: "c", items: items}, TopicID: 1},
	}, testNow).Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows, _ := st.ListRunDigests(ctx, testNow.Unix())
	if len(rows) != 1 || rows[0].ItemCount != 2 {
		t.Fatalf("rows = %+v, want one row with both items (course не выбрасывается)", rows)
	}
	ditems, _ := st.ListDigestItems(ctx, rows[0].ID)
	if len(ditems) != 2 {
		t.Fatalf("digest_items = %+v, want 2", ditems)
	}
	if ditems[0].Class != llm.ClassCourse || ditems[0].Summary != courseSummary {
		t.Errorf("course item = (%q,%q), want (course,%q)", ditems[0].Class, ditems[0].Summary, courseSummary)
	}
	if strings.Contains(ditems[0].Summary, "\n") {
		t.Errorf("course summary is not one phrase: %q", ditems[0].Summary)
	}
	if ditems[1].Class != llm.ClassOther {
		t.Errorf("second item class = %q, want other", ditems[1].Class)
	}
	states, _ := st.GetMessageStates(ctx, dedupKeys(items))
	if s := states[items[0].DedupKey()]; s.Status != "done" || s.Class != llm.ClassCourse {
		t.Errorf("course state = %+v, want done/course", s)
	}
}

// --- Этап 9A, кейс 5: крах между UpsertMessageState и доставкой ---
// Повторный Run НЕ зовёт LLM по обработанному (счётчик), но доставляет.

func TestPipelineRun_CrashAfterCacheBeforeDelivery(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	cls := &fakeClassifier{}
	items := makeItems("a", 2)
	srcs := []TrackedSource{{Src: fakeSource{name: "a", items: items}, TopicID: 1}}

	// Прогон 1: вызов A успешен, кеш записан, доставка «падает» (крах).
	badPoster := &fakePoster{err: errors.New("network down")}
	err := newTestPipeline(st, cls, badPoster, srcs, testNow).Run(ctx)
	if err == nil {
		t.Fatal("Run(1) = nil, want delivery error")
	}
	states, err := st.GetMessageStates(ctx, dedupKeys(items))
	if err != nil {
		t.Fatalf("GetMessageStates: %v", err)
	}
	if len(states) != 2 {
		t.Fatalf("states = %d, want 2 (кеш пережил крах)", len(states))
	}

	// Прогон 2 («рестарт»): доставка работает; НОЛЬ новых вызовов A —
	// summary/class берутся из кеша message_state.
	poster := &fakePoster{}
	if err := newTestPipeline(st, cls, poster, srcs, testNow.Add(time.Hour)).Run(ctx); err != nil {
		t.Fatalf("Run(2): %v", err)
	}
	if cls.callCount() != 1 {
		t.Errorf("classify calls = %d, want 1 (кеш: LLM не переспрашивался)", cls.callCount())
	}
	if poster.calls() != 1 {
		t.Fatalf("poster calls = %d, want 1", poster.calls())
	}
	rows, _ := st.ListRunDigests(ctx, testNow.Add(time.Hour).Unix())
	if len(rows) != 1 || !rows[0].Delivered {
		t.Fatalf("rows = %+v, want one delivered row of run 2", rows)
	}
	ditems, _ := st.ListDigestItems(ctx, rows[0].ID)
	if len(ditems) != 2 || ditems[0].Summary != "s1" || ditems[1].Summary != "s2" {
		t.Errorf("digest_items = %+v, want cached summaries s1,s2", ditems)
	}
	for _, di := range ditems {
		if di.Class != llm.ClassOther {
			t.Errorf("item %s class = %q, want other", di.Title, di.Class)
		}
	}
	fresh, _ := st.FilterUnseen(ctx, items)
	if len(fresh) != 0 {
		t.Errorf("fresh = %+v, want empty after run 2", fresh)
	}
}

// --- Этап 9A, кейс 5: Degraded-результат не кэшируется, следующий прогон
// переспрашивает LLM ---

func TestPipelineRun_DegradedNotCachedAndReasked(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	cls := &fakeClassifier{byTitle: map[string]fakeOutcome{
		"a-T1": {summary: "lead-1", degraded: true},
		"a-T2": {summary: "lead-2", degraded: true},
	}}
	items := makeItems("a", 2)
	srcs := []TrackedSource{{Src: fakeSource{name: "a", items: items}, TopicID: 1}}

	// Прогон 1: деградация + сбой доставки → элементы не seen, кеш ПУСТ.
	badPoster := &fakePoster{err: errors.New("network down")}
	if err := newTestPipeline(st, cls, badPoster, srcs, testNow).Run(ctx); err == nil {
		t.Fatal("Run(1) = nil, want delivery error")
	}
	states, err := st.GetMessageStates(ctx, dedupKeys(items))
	if err != nil {
		t.Fatalf("GetMessageStates: %v", err)
	}
	if len(states) != 0 {
		t.Fatalf("states = %+v, want empty (Degraded не кэшируется)", states)
	}

	// Прогон 2: LLM переспрашивается (счётчик растёт), результат доставлен
	// с class=other и extractLead-саммари.
	poster := &fakePoster{}
	if err := newTestPipeline(st, cls, poster, srcs, testNow.Add(time.Hour)).Run(ctx); err != nil {
		t.Fatalf("Run(2): %v", err)
	}
	if cls.callCount() != 2 {
		t.Errorf("classify calls = %d, want 2 (деградация переспрошена)", cls.callCount())
	}
	rows, _ := st.ListRunDigests(ctx, testNow.Add(time.Hour).Unix())
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want 1", rows)
	}
	ditems, _ := st.ListDigestItems(ctx, rows[0].ID)
	if len(ditems) != 2 || ditems[0].Summary != "lead-1" || ditems[1].Summary != "lead-2" {
		t.Fatalf("digest_items = %+v, want degraded summaries lead-1,lead-2", ditems)
	}
	for _, di := range ditems {
		if di.Class != llm.ClassOther {
			t.Errorf("item %s class = %q, want other (degraded)", di.Title, di.Class)
		}
	}
	states, _ = st.GetMessageStates(ctx, dedupKeys(items))
	if len(states) != 0 {
		t.Errorf("states = %+v, want still empty after degraded run 2", states)
	}
}

// --- точечные фейки фиксов 9A (M1/L4/T4) ---

// partialFailClassifier — M1: для префикса «a» возвращает ЧАСТИЧНЫЙ результат
// (первый элемент) вместе с транспорт-ошибкой; остальные темы обслуживает как
// fakeClassifier по умолчанию.
type partialFailClassifier struct{}

func (f *partialFailClassifier) ClassifySummarize(ctx context.Context,
	items []llm.ClassifyItem) (llm.ClassifyResult, error) {
	if titlePrefix(items[0].Title) == "a" {
		return llm.ClassifyResult{
			Items:    []llm.ItemClassSummary{{Index: 1, Class: llm.ClassNews, Summary: "p1"}},
			Degraded: []bool{false},
		}, errors.New("boom after first chunk")
	}
	res := llm.ClassifyResult{
		Items:    make([]llm.ItemClassSummary, len(items)),
		Degraded: make([]bool, len(items)),
	}
	for i := range items {
		res.Items[i] = llm.ItemClassSummary{Index: i + 1, Class: llm.ClassOther, Summary: "b" + strconv.Itoa(i+1)}
	}
	return res, nil
}

// shortResultClassifier — L4: при err==nil возвращает результат КОРОЧЕ входа
// (нарушение контракта ClassifySummarizer).
type shortResultClassifier struct{}

func (f *shortResultClassifier) ClassifySummarize(ctx context.Context,
	items []llm.ClassifyItem) (llm.ClassifyResult, error) {
	return llm.ClassifyResult{
		Items:    []llm.ItemClassSummary{{Index: 1, Class: llm.ClassNews, Summary: "s"}},
		Degraded: []bool{false},
	}, nil
}

// failStoreOps — T4: точечные сбои store-операций 9A. UpsertMessageState
// падает только по конкретному dedup_key, чтобы вторая тема выживала.
type failStoreOps struct {
	storage.Store
	failGet       bool
	failUpsertKey string
	failMarkSeen  bool
}

func (s *failStoreOps) GetMessageStates(ctx context.Context, keys []string) (map[string]storage.MessageState, error) {
	if s.failGet {
		return nil, errors.New("get states boom")
	}
	return s.Store.GetMessageStates(ctx, keys)
}

func (s *failStoreOps) UpsertMessageState(ctx context.Context, states []storage.MessageState) error {
	for _, st := range states {
		if s.failUpsertKey != "" && st.DedupKey == s.failUpsertKey {
			return errors.New("upsert boom")
		}
	}
	return s.Store.UpsertMessageState(ctx, states)
}

func (s *failStoreOps) MarkSeenBatch(ctx context.Context, keys []string) error {
	if s.failMarkSeen {
		return errors.New("mark seen boom")
	}
	return s.Store.MarkSeenBatch(ctx, keys)
}

// --- M1: частичный результат при транспорт-сбое кэшируется до пропуска темы ---

func TestPipelineRun_PartialResultCachedOnClassifyError(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	fiveID, err := st.CreateTopic(ctx, "Five")
	if err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	cls := &partialFailClassifier{}
	poster := &fakePoster{}
	aItems := makeItems("a", 2)
	bItems := makeItems("b", 1)
	p := newTestPipeline(st, cls, poster, []TrackedSource{
		{Src: fakeSource{name: "a", items: aItems}, TopicID: 1},
		{Src: fakeSource{name: "b", items: bItems}, TopicID: fiveID},
	}, testNow)
	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run = %v, want nil (упала только тема a)", err)
	}
	// Упавшая тема — не в оглавлении и не в digests.
	if poster.calls() != 1 || strings.Contains(poster.texts[0], "Без темы") {
		t.Fatalf("TOC must contain only Five:\n%s", poster.texts[0])
	}
	rows, err := st.ListRunDigests(ctx, testNow.Unix())
	if err != nil {
		t.Fatalf("ListRunDigests: %v", err)
	}
	if len(rows) != 1 || rows[0].TopicID != fiveID {
		t.Fatalf("rows = %+v, want a single Five row", rows)
	}
	// Частичный результат закеширован: a-T1 done/news/p1, a-T2 — нет.
	states, err := st.GetMessageStates(ctx, dedupKeys(aItems))
	if err != nil {
		t.Fatalf("GetMessageStates: %v", err)
	}
	if len(states) != 1 {
		t.Fatalf("states = %+v, want exactly 1 (partial cache)", states)
	}
	if s := states[aItems[0].DedupKey()]; s.Status != "done" || s.Summary != "p1" || s.Class != llm.ClassNews {
		t.Errorf("a-T1 state = %+v, want done/news/p1", s)
	}
	// Элементы упавшей темы НЕ seen.
	fresh, err := st.FilterUnseen(ctx, aItems)
	if err != nil {
		t.Fatalf("FilterUnseen: %v", err)
	}
	if len(fresh) != 2 {
		t.Errorf("fresh = %+v, want both a-items unseen", fresh)
	}
}

// --- L3: topic_id в message_state — нормализованный (TopicID=0 → 1) ---

func TestPipelineRun_StateTopicIDNormalized(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	cls := &fakeClassifier{}
	poster := &fakePoster{}
	items := makeItems("a", 1)
	p := newTestPipeline(st, cls, poster, []TrackedSource{
		{Src: fakeSource{name: "a", items: items}, SourceID: 7, TopicID: 0}, // «Без темы»
	}, testNow)
	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	states, err := st.GetMessageStates(ctx, dedupKeys(items))
	if err != nil {
		t.Fatalf("GetMessageStates: %v", err)
	}
	s := states[items[0].DedupKey()]
	if s.TopicID != 1 {
		t.Errorf("state TopicID = %d, want 1 (нормализованный «Без темы»)", s.TopicID)
	}
}

// --- L4: короткий результат при err==nil → тема пропущена, без panic ---

func TestPipelineRun_ContractViolationSkipsTopic(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	poster := &fakePoster{}
	items := makeItems("a", 2) // фейк вернёт только 1 элемент
	p := newTestPipeline(st, &shortResultClassifier{}, poster, []TrackedSource{
		{Src: fakeSource{name: "a", items: items}, TopicID: 1},
	}, testNow)
	err := p.Run(ctx) // guard: без него — panic index out of range
	if err == nil {
		t.Fatal("Run = nil, want error (единственная тема упала на guard)")
	}
	if poster.calls() != 0 {
		t.Errorf("poster calls = %d, want 0", poster.calls())
	}
	states, err := st.GetMessageStates(ctx, dedupKeys(items))
	if err != nil {
		t.Fatalf("GetMessageStates: %v", err)
	}
	if len(states) != 0 {
		t.Errorf("states = %+v, want empty (guard до кэширования)", states)
	}
}

// --- L7: write-back кеша — кросс-топик дубль dedup_key не уходит в LLM повторно ---

func TestPipelineRun_CrossTopicDuplicateClassifiedOnce(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	fiveID, err := st.CreateTopic(ctx, "Five")
	if err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	const dupURL = "https://dup.test/x"
	dupA := feed.Item{URL: dupURL, Title: "dup-A", Text: "общий URL, тема 1"}
	dupB := feed.Item{URL: dupURL, Title: "dup-B", Text: "общий URL, тема Five"}
	cls := &fakeClassifier{}
	poster := &fakePoster{}
	p := newTestPipeline(st, cls, poster, []TrackedSource{
		{Src: fakeSource{name: "a", items: []feed.Item{dupA}}, TopicID: 1},
		{Src: fakeSource{name: "b", items: []feed.Item{dupB}}, TopicID: fiveID},
	}, testNow)
	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if cls.callCount() != 1 {
		t.Fatalf("classify calls = %d, want 1 (во второй теме элемент из кеша)", cls.callCount())
	}
	if len(cls.batches) != 1 || len(cls.batches[0]) != 1 || cls.batches[0][0].Title != "dup-A" {
		t.Fatalf("batches = %+v, want a single [dup-A] batch", cls.batches)
	}
	rows, err := st.ListRunDigests(ctx, testNow.Unix())
	if err != nil {
		t.Fatalf("ListRunDigests: %v", err)
	}
	if len(rows) != 2 || rows[1].TopicID != fiveID {
		t.Fatalf("rows = %+v, want topics [1 Five]", rows)
	}
	ditems, err := st.ListDigestItems(ctx, rows[1].ID)
	if err != nil {
		t.Fatalf("ListDigestItems: %v", err)
	}
	if len(ditems) != 1 || ditems[0].Title != "dup-B" || ditems[0].Summary != "s1" {
		t.Errorf("Five digest_items = %+v, want dup-B с summary из кеша (s1)", ditems)
	}
}

// --- T1: смешанный кеш в одной теме (2 закешированных + 1 новый) ---

func TestPipelineRun_MixedCacheAndFreshInOneTopic(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	cls := &fakeClassifier{byTitle: map[string]fakeOutcome{
		"a-T3": {summary: "new3"}, // у нового элемента summary, отличный от s1/s2
	}}

	// Прогон 1: 2 элемента, доставка ПАДАЕТ → кеш done на 2, элементы не seen.
	items1 := makeItems("a", 2)
	badPoster := &fakePoster{err: errors.New("network down")}
	srcs1 := []TrackedSource{{Src: fakeSource{name: "a", items: items1}, TopicID: 1}}
	if err := newTestPipeline(st, cls, badPoster, srcs1, testNow).Run(ctx); err == nil {
		t.Fatal("Run(1) = nil, want delivery error")
	}

	// Прогон 2: тот же источник отдаёт 3 элемента (2 прежних + 1 новый;
	// FilterUnseen пропустит все 3 — seen не помечены).
	items2 := makeItems("a", 3)
	poster := &fakePoster{}
	srcs2 := []TrackedSource{{Src: fakeSource{name: "a", items: items2}, TopicID: 1}}
	if err := newTestPipeline(st, cls, poster, srcs2, testNow.Add(time.Hour)).Run(ctx); err != nil {
		t.Fatalf("Run(2): %v", err)
	}
	if cls.callCount() != 2 {
		t.Fatalf("classify calls = %d, want 2 (1 в каждом прогоне)", cls.callCount())
	}
	// Вызов A в прогоне 2 — РОВНО с 1 элементом (новый a-T3).
	if len(cls.batches) != 2 || len(cls.batches[1]) != 1 || cls.batches[1][0].Title != "a-T3" {
		t.Fatalf("batches = %+v, want run2 batch = [a-T3] only", cls.batches)
	}
	rows, err := st.ListRunDigests(ctx, testNow.Add(time.Hour).Unix())
	if err != nil {
		t.Fatalf("ListRunDigests: %v", err)
	}
	if len(rows) != 1 || rows[0].ItemCount != 3 || !rows[0].Delivered {
		t.Fatalf("rows = %+v, want one delivered row of run 2 with ItemCount=3", rows)
	}
	ditems, err := st.ListDigestItems(ctx, rows[0].ID)
	if err != nil {
		t.Fatalf("ListDigestItems: %v", err)
	}
	// Поэлементный маппинг pos/unclsAt без off-by-one: закешированные summary
	// остаются на своих местах, новый получает свой.
	wantT := []string{"a-T1", "a-T2", "a-T3"}
	wantS := []string{"s1", "s2", "new3"}
	if len(ditems) != 3 {
		t.Fatalf("digest_items = %+v, want 3", ditems)
	}
	for i := range ditems {
		if ditems[i].Title != wantT[i] || ditems[i].Summary != wantS[i] {
			t.Errorf("item[%d] = (%q,%q), want (%q,%q)",
				i, ditems[i].Title, ditems[i].Summary, wantT[i], wantS[i])
		}
	}
	fresh, err := st.FilterUnseen(ctx, items2)
	if err != nil {
		t.Fatalf("FilterUnseen: %v", err)
	}
	if len(fresh) != 0 {
		t.Errorf("fresh = %+v, want empty after run 2", fresh)
	}
}

// --- T5: юнит isClassifiedState ---

func TestIsClassifiedState(t *testing.T) {
	cases := []struct {
		status string
		want   bool
	}{
		{stateDone, true},
		{stateDroppedAd, true},
		{stateDroppedDup, true},
		{"new", false},
		{"failed", false},
		{"", false},
		{"мусор", false},
	}
	for _, c := range cases {
		if got := isClassifiedState(c.status); got != c.want {
			t.Errorf("isClassifiedState(%q) = %v, want %v", c.status, got, c.want)
		}
	}
}

// --- T4: failure-пути store-вызовов 9A ---

func TestPipelineRun_GetMessageStatesErrorIsFatal(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	fso := &failStoreOps{Store: st, failGet: true}
	poster := &fakePoster{}
	items := makeItems("a", 1)
	p := newTestPipeline(fso, &fakeClassifier{}, poster, []TrackedSource{
		{Src: fakeSource{name: "a", items: items}, TopicID: 1},
	}, testNow)
	err := p.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "get message states") {
		t.Fatalf("Run err = %v, want «get message states: …»", err)
	}
	if poster.calls() != 0 {
		t.Errorf("poster calls = %d, want 0", poster.calls())
	}
}

func TestPipelineRun_UpsertStateErrorSkipsTopicOnly(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	fiveID, err := st.CreateTopic(ctx, "Five")
	if err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	aItems := makeItems("a", 1)
	bItems := makeItems("b", 1)
	fso := &failStoreOps{Store: st, failUpsertKey: aItems[0].DedupKey()}
	poster := &fakePoster{}
	p := newTestPipeline(fso, &fakeClassifier{}, poster, []TrackedSource{
		{Src: fakeSource{name: "a", items: aItems}, TopicID: 1},
		{Src: fakeSource{name: "b", items: bItems}, TopicID: fiveID},
	}, testNow)
	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run = %v, want nil (упала только тема a)", err)
	}
	if poster.calls() != 1 || strings.Contains(poster.texts[0], "Без темы") {
		t.Fatalf("TOC must contain only Five:\n%s", poster.texts[0])
	}
	// Элементы упавшей темы НЕ seen и НЕ в кеше.
	fresh, err := st.FilterUnseen(ctx, aItems)
	if err != nil {
		t.Fatalf("FilterUnseen: %v", err)
	}
	if len(fresh) != 1 {
		t.Errorf("fresh = %+v, want a-item unseen", fresh)
	}
	states, err := st.GetMessageStates(ctx, dedupKeys(aItems))
	if err != nil {
		t.Fatalf("GetMessageStates: %v", err)
	}
	if len(states) != 0 {
		t.Errorf("states = %+v, want empty (upsert упал)", states)
	}
}

func TestPipelineRun_MarkSeenBatchErrorIsLogOnly(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	fso := &failStoreOps{Store: st, failMarkSeen: true}
	cls := &fakeClassifier{byTitle: map[string]fakeOutcome{
		"ad-T1": {class: llm.ClassAd},
	}}
	items := append(makeItems("ad", 1), makeItems("ok", 1)...)
	poster := &fakePoster{}
	p := newTestPipeline(fso, cls, poster, []TrackedSource{
		{Src: fakeSource{name: "a", items: items}, TopicID: 1},
	}, testNow)
	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run = %v, want nil (MarkSeenBatch — log-only)", err)
	}
	if poster.calls() != 1 {
		t.Fatalf("poster calls = %d, want 1 (доставка не страдает)", poster.calls())
	}
	// Ad не помечен seen — следующий прогон повторно выбросит его уже из кеша.
	fresh, err := st.FilterUnseen(ctx, items)
	if err != nil {
		t.Fatalf("FilterUnseen: %v", err)
	}
	if len(fresh) != 1 || fresh[0].Title != "ad-T1" {
		t.Errorf("fresh = %+v, want only the unmarked ad", fresh)
	}
}
