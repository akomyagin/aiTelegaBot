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
	"unicode/utf8"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
	"github.com/akomyagin/aiTelegaBot/internal/linkfetch"
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

// --- Этап 9B (план, тест-кейсы 5–6): фейки шага 4c со счётчиками ---

// fakeLinkFetcher — фейк LinkFetcher: счётчик вызовов, порядок URL, отказы по
// failURLs (любая ошибка Fetch → pipeline пишет kind=unavailable, без ретрая).
type fakeLinkFetcher struct {
	mu       sync.Mutex
	calls    int
	urls     []string
	failURLs map[string]bool
}

func (f *fakeLinkFetcher) Fetch(ctx context.Context, url string) (linkfetch.Page, error) {
	f.mu.Lock()
	f.calls++
	f.urls = append(f.urls, url)
	fail := f.failURLs[url]
	f.mu.Unlock()
	if fail {
		return linkfetch.Page{}, errors.New("fetch boom: " + url)
	}
	return linkfetch.Page{URL: url, Title: "title " + url, Text: "text of " + url}, nil
}

func (f *fakeLinkFetcher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeLinkClassifier — фейк вызова E: счётчик, вид по URL (byURL), вид по
// паре (Msg,Link) 1-based (byPair — приоритетнее byURL; нужен для конфликта
// видов одного URL в разных сообщениях темы, ревью F1-iii), дефолт article;
// err рвёт вызов (кейс E-error).
type fakeLinkClassifier struct {
	mu     sync.Mutex
	calls  int
	byURL  map[string]string
	byPair map[[2]int]string
	err    error
}

func (f *fakeLinkClassifier) ClassifyLinks(ctx context.Context, msgs []llm.MsgLinks) ([]llm.LinkKind, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	out := make([]llm.LinkKind, 0, len(msgs))
	for m, msg := range msgs {
		for l, u := range msg.URLs {
			kind := llm.LinkKindArticle
			if v, ok := f.byPair[[2]int{m + 1, l + 1}]; ok {
				kind = v
			} else if v, ok := f.byURL[u]; ok {
				kind = v
			}
			out = append(out, llm.LinkKind{Msg: m + 1, Link: l + 1, Kind: kind})
		}
	}
	return out, nil
}

func (f *fakeLinkClassifier) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeLinkSummarizer — фейк вызова F: счётчик, саммари по URL (byURL), дефолт
// «сжато: <url>»; err рвёт вызов (кейс F-error).
type fakeLinkSummarizer struct {
	mu    sync.Mutex
	calls int
	byURL map[string]string
	err   error
}

func (f *fakeLinkSummarizer) SummarizeLinks(ctx context.Context, pages []llm.LinkPage) ([]llm.LinkSummary, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	out := make([]llm.LinkSummary, 0, len(pages))
	for i, pg := range pages {
		s := "сжато: " + pg.URL
		if v, ok := f.byURL[pg.URL]; ok {
			s = v
		}
		out = append(out, llm.LinkSummary{Index: i + 1, Summary: s})
	}
	return out, nil
}

func (f *fakeLinkSummarizer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// linkTestPipeline — Pipeline 9A (newTestPipeline) + поля шага 4c Этапа 9B.
func linkTestPipeline(st storage.Store, cls llm.ClassifySummarizer, poster MessagePoster,
	srcs []TrackedSource, e llm.LinkClassifier, f llm.LinkSummarizer, lf LinkFetcher,
	perMsg, totalMax int) *Pipeline {
	p := newTestPipeline(st, cls, poster, srcs, testNow)
	p.LinkClassify, p.LinkSummary, p.LinkFetch = e, f, lf
	p.LinkPerMessageMax, p.LinkFetchTotalMax = perMsg, totalMax
	return p
}

// linkItem — tg-элемент со ссылкой-статьёй в тексте. Item.URL — t.me-ссылка:
// extractURLs её отбрасывает (не внешняя), dedup_key элементов разных каналов
// различается.
func linkItem(ch, id, title, articleURL string) feed.Item {
	return feed.Item{
		Kind:  "tg_public",
		ID:    id,
		URL:   "https://t.me/" + ch + "/" + id,
		Title: title,
		Text:  "пост про " + articleURL,
	}
}

// 9B кейс 5: одна статья из двух каналов — разные dedup_key, ОДИН URL, одна
// тема: Fetcher вызван ОДИН раз, link_cache одна строка, ОБА сообщения получили
// ↳-саммари; message_state.summary — базовое, без ↳.
func TestPipelineRun_SharedArticleFetchedOnce(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	const art = "https://art.test/x"
	it1 := linkItem("ch1", "1", "c1-T1", art)
	it2 := linkItem("ch2", "2", "c2-T1", art)
	if it1.DedupKey() == it2.DedupKey() {
		t.Fatalf("dedup keys collide: %q", it1.DedupKey())
	}
	e := &fakeLinkClassifier{}
	f := &fakeLinkSummarizer{}
	lf := &fakeLinkFetcher{}
	poster := &fakePoster{}
	srcs := []TrackedSource{
		{Src: fakeSource{name: "ch1", items: []feed.Item{it1}}, SourceID: 1, TopicID: 1},
		{Src: fakeSource{name: "ch2", items: []feed.Item{it2}}, SourceID: 2, TopicID: 1},
	}
	if err := linkTestPipeline(st, &fakeClassifier{}, poster, srcs, e, f, lf, 5, 30).Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if e.callCount() != 1 {
		t.Errorf("E calls = %d, want 1 (один вызов на тему)", e.callCount())
	}
	if lf.callCount() != 1 {
		t.Errorf("fetch calls = %d, want 1 (общий URL фетчится один раз)", lf.callCount())
	}
	if f.callCount() != 1 {
		t.Errorf("F calls = %d, want 1", f.callCount())
	}
	rows, err := st.ListRunDigests(ctx, testNow.Unix())
	if err != nil {
		t.Fatalf("ListRunDigests: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want 1", rows)
	}
	ditems, err := st.ListDigestItems(ctx, rows[0].ID)
	if err != nil {
		t.Fatalf("ListDigestItems: %v", err)
	}
	if len(ditems) != 2 {
		t.Fatalf("digest_items = %+v, want 2", ditems)
	}
	wantSuffix := "\n↳ сжато: " + art
	for _, di := range ditems {
		if !strings.HasSuffix(di.Summary, wantSuffix) {
			t.Errorf("summary %q lacks %q", di.Summary, wantSuffix)
		}
	}
	cache, err := st.GetLinkCache(ctx, []string{art})
	if err != nil {
		t.Fatalf("GetLinkCache: %v", err)
	}
	if len(cache) != 1 {
		t.Fatalf("link_cache rows = %+v, want exactly 1", cache)
	}
	if row := cache[art]; row.Kind != llm.LinkKindArticle ||
		row.Summary != "сжато: "+art || row.Title != "title "+art {
		t.Errorf("cache row = %+v, want article + fetched title + F summary", row)
	}
	for _, it := range []feed.Item{it1, it2} {
		links, err := st.ListItemLinks(ctx, it.DedupKey())
		if err != nil {
			t.Fatalf("ListItemLinks: %v", err)
		}
		if len(links) != 1 || links[0].URL != art ||
			links[0].Kind != llm.LinkKindArticle || links[0].Ord != 1 {
			t.Errorf("item_links(%s) = %+v, want one article ord=1", it.DedupKey(), links)
		}
	}
	states, err := st.GetMessageStates(ctx, dedupKeys([]feed.Item{it1, it2}))
	if err != nil {
		t.Fatalf("GetMessageStates: %v", err)
	}
	if len(states) != 2 {
		t.Fatalf("states = %+v, want 2", states)
	}
	for _, s := range states {
		if strings.Contains(s.Summary, "↳") {
			t.Errorf("message_state summary %q must stay base (no ↳)", s.Summary)
		}
	}
}

// 9B кейс 5: MESSAGE-level skip (ревью M2) — СООБЩЕНИЕ с class ∈ {course, ad}
// из вызова A вообще не доходит до шага 4c: extractURLs не запускается, E/fetch
// не вызываются, item_links пуст; ad отсеян до 4c — то же. Не путать с
// LINK-level skip (kind ∈ {course, ad} из вызова E внутри news-сообщения) —
// см. TestPipelineRun_CourseLinkKindCachedWithoutFetch.
func TestPipelineRun_CourseAndAdMessageSkipsLinkStep(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	const courseArt = "https://course.test/go"
	const adArt = "https://ad.test/sale"
	cIt := linkItem("c", "1", "c-T1", courseArt)
	adIt := linkItem("ad", "2", "ad-T1", adArt)
	cls := &fakeClassifier{byTitle: map[string]fakeOutcome{
		"c-T1":  {class: llm.ClassCourse, summary: "Курс: Go — основы"},
		"ad-T1": {class: llm.ClassAd},
	}}
	e := &fakeLinkClassifier{}
	f := &fakeLinkSummarizer{}
	lf := &fakeLinkFetcher{}
	poster := &fakePoster{}
	srcs := []TrackedSource{{Src: fakeSource{name: "c", items: []feed.Item{cIt, adIt}}, TopicID: 1}}
	if err := linkTestPipeline(st, cls, poster, srcs, e, f, lf, 5, 30).Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if e.callCount() != 0 || lf.callCount() != 0 || f.callCount() != 0 {
		t.Errorf("E/fetch/F calls = %d/%d/%d, want 0/0/0",
			e.callCount(), lf.callCount(), f.callCount())
	}
	for _, it := range []feed.Item{cIt, adIt} {
		links, err := st.ListItemLinks(ctx, it.DedupKey())
		if err != nil {
			t.Fatalf("ListItemLinks: %v", err)
		}
		if len(links) != 0 {
			t.Errorf("item_links(%s) = %+v, want empty", it.DedupKey(), links)
		}
	}
	cache, err := st.GetLinkCache(ctx, []string{courseArt, adArt})
	if err != nil {
		t.Fatalf("GetLinkCache: %v", err)
	}
	if len(cache) != 0 {
		t.Errorf("link_cache = %+v, want empty", cache)
	}
	rows, err := st.ListRunDigests(ctx, testNow.Unix())
	if err != nil {
		t.Fatalf("ListRunDigests: %v", err)
	}
	if len(rows) != 1 || rows[0].ItemCount != 1 {
		t.Fatalf("rows = %+v, want one row ItemCount=1 (course; ad выброшен)", rows)
	}
	ditems, err := st.ListDigestItems(ctx, rows[0].ID)
	if err != nil {
		t.Fatalf("ListDigestItems: %v", err)
	}
	if len(ditems) != 1 || ditems[0].Title != "c-T1" ||
		strings.Contains(ditems[0].Summary, "↳") {
		t.Errorf("digest_items = %+v, want lone course item without ↳", ditems)
	}
}

// 9B кейс 5: LinkFetchTotalMax=2 и 3 статьи → Fetcher вызван 2 раза, третья
// ссылка есть в item_links, но без строки link_cache и без ↳ (деградация).
func TestPipelineRun_FetchBudgetIsPerRun(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	arts := []string{"https://a1.test/x", "https://a2.test/x", "https://a3.test/x"}
	items := make([]feed.Item, 0, 3)
	for i, a := range arts {
		items = append(items, linkItem("b", strconv.Itoa(i+1),
			"b-T"+strconv.Itoa(i+1), a))
	}
	e := &fakeLinkClassifier{}
	f := &fakeLinkSummarizer{}
	lf := &fakeLinkFetcher{}
	poster := &fakePoster{}
	srcs := []TrackedSource{{Src: fakeSource{name: "b", items: items}, TopicID: 1}}
	if err := linkTestPipeline(st, &fakeClassifier{}, poster, srcs, e, f, lf, 5, 2).Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if lf.callCount() != 2 {
		t.Fatalf("fetch calls = %d, want 2 (общий бюджет прогона)", lf.callCount())
	}
	cache, err := st.GetLinkCache(ctx, arts)
	if err != nil {
		t.Fatalf("GetLinkCache: %v", err)
	}
	if len(cache) != 2 {
		t.Errorf("link_cache rows = %+v, want 2 (третья без строки)", cache)
	}
	if _, hit := cache[arts[2]]; hit {
		t.Errorf("третья ссылка не должна быть в link_cache: %+v", cache[arts[2]])
	}
	for i, it := range items {
		links, err := st.ListItemLinks(ctx, it.DedupKey())
		if err != nil {
			t.Fatalf("ListItemLinks: %v", err)
		}
		if len(links) != 1 || links[0].URL != arts[i] || links[0].Kind != llm.LinkKindArticle {
			t.Errorf("item_links(%s) = %+v, want one article %s", it.DedupKey(), links, arts[i])
		}
	}
	rows, err := st.ListRunDigests(ctx, testNow.Unix())
	if err != nil {
		t.Fatalf("ListRunDigests: %v", err)
	}
	ditems, err := st.ListDigestItems(ctx, rows[0].ID)
	if err != nil {
		t.Fatalf("ListDigestItems: %v", err)
	}
	if len(ditems) != 3 {
		t.Fatalf("digest_items = %+v, want 3", ditems)
	}
	for i, di := range ditems {
		has := strings.Contains(di.Summary, "↳")
		if want := i < 2; has != want {
			t.Errorf("item[%d] has↳ = %v, want %v (бюджет 2)", i, has, want)
		}
	}
}

// 9B кейс 5: offline — LinkFetch=nil, E=Offline → item_links kind=other,
// fetch/F не вызываются, link_cache пуст, дайджест цел (summary без ↳).
func TestPipelineRun_OfflineLinkDegradation(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	const art = "https://art.test/off"
	it := linkItem("o", "1", "o-T1", art)
	f := &fakeLinkSummarizer{} // в offline-wiring F есть (Offline), но не вызывается
	poster := &fakePoster{}
	srcs := []TrackedSource{{Src: fakeSource{name: "o", items: []feed.Item{it}}, TopicID: 1}}
	p := linkTestPipeline(st, &fakeClassifier{}, poster, srcs, llm.NewOffline(), f, nil, 5, 30)
	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.callCount() != 0 {
		t.Errorf("F calls = %d, want 0 (нет fetch — нет свежих страниц)", f.callCount())
	}
	links, err := st.ListItemLinks(ctx, it.DedupKey())
	if err != nil {
		t.Fatalf("ListItemLinks: %v", err)
	}
	if len(links) != 1 || links[0].Kind != llm.LinkKindOther {
		t.Errorf("item_links = %+v, want one other (Offline-E)", links)
	}
	cache, err := st.GetLinkCache(ctx, []string{art})
	if err != nil {
		t.Fatalf("GetLinkCache: %v", err)
	}
	if len(cache) != 0 {
		t.Errorf("link_cache = %+v, want empty offline", cache)
	}
	if poster.calls() != 1 {
		t.Fatalf("poster calls = %d, want 1 (дайджест цел)", poster.calls())
	}
	rows, err := st.ListRunDigests(ctx, testNow.Unix())
	if err != nil {
		t.Fatalf("ListRunDigests: %v", err)
	}
	ditems, err := st.ListDigestItems(ctx, rows[0].ID)
	if err != nil {
		t.Fatalf("ListDigestItems: %v", err)
	}
	if len(ditems) != 1 || ditems[0].Summary != "s1" {
		t.Errorf("digest_items = %+v, want base summary s1 without ↳", ditems)
	}
}

// 9B, решение диспетчера (E-error): ошибка вызова E → ВСЕ пары kind=other в
// item_links, Fetcher/F НЕ вызываются, дайджест цел, Run=nil.
func TestPipelineRun_LinkClassifyErrorDegradesToOther(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	const art = "https://art.test/e"
	it := linkItem("e", "1", "e-T1", art)
	e := &fakeLinkClassifier{err: errors.New("E boom")}
	f := &fakeLinkSummarizer{}
	lf := &fakeLinkFetcher{}
	poster := &fakePoster{}
	srcs := []TrackedSource{{Src: fakeSource{name: "e", items: []feed.Item{it}}, TopicID: 1}}
	if err := linkTestPipeline(st, &fakeClassifier{}, poster, srcs, e, f, lf, 5, 30).Run(ctx); err != nil {
		t.Fatalf("Run = %v, want nil (E-error — деградация)", err)
	}
	if e.callCount() != 1 {
		t.Errorf("E calls = %d, want 1", e.callCount())
	}
	if lf.callCount() != 0 || f.callCount() != 0 {
		t.Errorf("fetch/F calls = %d/%d, want 0/0", lf.callCount(), f.callCount())
	}
	links, err := st.ListItemLinks(ctx, it.DedupKey())
	if err != nil {
		t.Fatalf("ListItemLinks: %v", err)
	}
	if len(links) != 1 || links[0].Kind != llm.LinkKindOther || links[0].Ord != 1 {
		t.Errorf("item_links = %+v, want one other ord=1", links)
	}
	if poster.calls() != 1 {
		t.Fatalf("poster calls = %d, want 1 (сообщение осталось в дайджесте)", poster.calls())
	}
	rows, err := st.ListRunDigests(ctx, testNow.Unix())
	if err != nil {
		t.Fatalf("ListRunDigests: %v", err)
	}
	ditems, err := st.ListDigestItems(ctx, rows[0].ID)
	if err != nil {
		t.Fatalf("ListDigestItems: %v", err)
	}
	if len(ditems) != 1 || ditems[0].Summary != "s1" {
		t.Errorf("digest_items = %+v, want base s1 without ↳", ditems)
	}
}

// 9B: ошибка вызова F → строки link_cache с пустым summary, дайджест цел
// без ↳, Run=nil (§4.3).
func TestPipelineRun_LinkSummarizeErrorKeepsEmptySummary(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	const art = "https://art.test/f"
	it := linkItem("g", "1", "g-T1", art)
	e := &fakeLinkClassifier{}
	f := &fakeLinkSummarizer{err: errors.New("F boom")}
	lf := &fakeLinkFetcher{}
	poster := &fakePoster{}
	srcs := []TrackedSource{{Src: fakeSource{name: "g", items: []feed.Item{it}}, TopicID: 1}}
	if err := linkTestPipeline(st, &fakeClassifier{}, poster, srcs, e, f, lf, 5, 30).Run(ctx); err != nil {
		t.Fatalf("Run = %v, want nil (F-error — деградация)", err)
	}
	if lf.callCount() != 1 || f.callCount() != 1 {
		t.Fatalf("fetch/F calls = %d/%d, want 1/1", lf.callCount(), f.callCount())
	}
	cache, err := st.GetLinkCache(ctx, []string{art})
	if err != nil {
		t.Fatalf("GetLinkCache: %v", err)
	}
	if len(cache) != 1 {
		t.Fatalf("link_cache = %+v, want 1 row", cache)
	}
	if row := cache[art]; row.Kind != llm.LinkKindArticle || row.Summary != "" ||
		row.Title != "title "+art {
		t.Errorf("cache row = %+v, want article + title + EMPTY summary", row)
	}
	if poster.calls() != 1 {
		t.Fatalf("poster calls = %d, want 1", poster.calls())
	}
	rows, err := st.ListRunDigests(ctx, testNow.Unix())
	if err != nil {
		t.Fatalf("ListRunDigests: %v", err)
	}
	ditems, err := st.ListDigestItems(ctx, rows[0].ID)
	if err != nil {
		t.Fatalf("ListDigestItems: %v", err)
	}
	if len(ditems) != 1 || strings.Contains(ditems[0].Summary, "↳") {
		t.Errorf("digest_items = %+v, want no ↳", ditems)
	}
}

// 9B: ошибка Fetch на один URL → строка kind=unavailable (без ретрая),
// остальные ссылки обработаны, дайджест цел.
func TestPipelineRun_FetchErrorMarksUnavailable(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	const bad = "https://bad.test/x"
	const good = "https://good.test/y"
	it1 := linkItem("h", "1", "h-T1", bad)
	it2 := linkItem("h", "2", "h-T2", good)
	e := &fakeLinkClassifier{}
	f := &fakeLinkSummarizer{}
	lf := &fakeLinkFetcher{failURLs: map[string]bool{bad: true}}
	poster := &fakePoster{}
	srcs := []TrackedSource{{Src: fakeSource{name: "h", items: []feed.Item{it1, it2}}, TopicID: 1}}
	if err := linkTestPipeline(st, &fakeClassifier{}, poster, srcs, e, f, lf, 5, 30).Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if lf.callCount() != 2 {
		t.Fatalf("fetch calls = %d, want 2 (по одной попытке на URL, без ретрая)", lf.callCount())
	}
	cache, err := st.GetLinkCache(ctx, []string{bad, good})
	if err != nil {
		t.Fatalf("GetLinkCache: %v", err)
	}
	if len(cache) != 2 {
		t.Fatalf("link_cache = %+v, want 2 rows", cache)
	}
	if row := cache[bad]; row.Kind != "unavailable" || row.Summary != "" {
		t.Errorf("bad row = %+v, want unavailable + empty summary", row)
	}
	if row := cache[good]; row.Kind != llm.LinkKindArticle || row.Summary == "" {
		t.Errorf("good row = %+v, want article + summary", row)
	}
	rows, err := st.ListRunDigests(ctx, testNow.Unix())
	if err != nil {
		t.Fatalf("ListRunDigests: %v", err)
	}
	ditems, err := st.ListDigestItems(ctx, rows[0].ID)
	if err != nil {
		t.Fatalf("ListDigestItems: %v", err)
	}
	if len(ditems) != 2 {
		t.Fatalf("digest_items = %+v, want 2", ditems)
	}
	if strings.Contains(ditems[0].Summary, "↳") {
		t.Errorf("failed-fetch item got ↳: %q", ditems[0].Summary)
	}
	if !strings.Contains(ditems[1].Summary, "↳ сжато: "+good) {
		t.Errorf("good item lacks ↳: %q", ditems[1].Summary)
	}
}

// 9B кейс 5: кеш прошлых прогонов — pre-seed link_cache с summary → ↳ без
// fetch (счётчик 0), F не вызывается.
func TestPipelineRun_CachedSummaryUsedWithoutFetch(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	const art = "https://art.test/old"
	if err := st.UpsertLinkCache(ctx, []storage.LinkCacheRow{
		{URL: art, Kind: llm.LinkKindArticle, Title: "старая", Summary: "кешированное саммари"},
	}); err != nil {
		t.Fatalf("UpsertLinkCache: %v", err)
	}
	it := linkItem("k", "1", "k-T1", art)
	e := &fakeLinkClassifier{}
	f := &fakeLinkSummarizer{}
	lf := &fakeLinkFetcher{}
	poster := &fakePoster{}
	srcs := []TrackedSource{{Src: fakeSource{name: "k", items: []feed.Item{it}}, TopicID: 1}}
	if err := linkTestPipeline(st, &fakeClassifier{}, poster, srcs, e, f, lf, 5, 30).Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if lf.callCount() != 0 {
		t.Errorf("fetch calls = %d, want 0 (кеш-хит не фетчится)", lf.callCount())
	}
	if f.callCount() != 0 {
		t.Errorf("F calls = %d, want 0 (нет свежих страниц)", f.callCount())
	}
	rows, err := st.ListRunDigests(ctx, testNow.Unix())
	if err != nil {
		t.Fatalf("ListRunDigests: %v", err)
	}
	ditems, err := st.ListDigestItems(ctx, rows[0].ID)
	if err != nil {
		t.Fatalf("ListDigestItems: %v", err)
	}
	if len(ditems) != 1 || ditems[0].Summary != "s1\n↳ кешированное саммари" {
		t.Errorf("digest_items = %+v, want «s1\\n↳ кешированное саммари»", ditems)
	}
}

// 9B кейс 6: ↳-конкатенация ≤600 рун с обрезкой на границе руны;
// message_state.summary остаётся базовым.
func TestPipelineRun_LinkSummaryConcatCappedAt600Runes(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	arts := []string{"https://l1.test/a", "https://l2.test/b", "https://l3.test/c"}
	long := strings.Repeat("длинное саммари ", 16) // 16 рун × 16 = 256 рун
	if n := utf8.RuneCountInString(long); n != 256 {
		t.Fatalf("fixture broken: %d runes, want 256", n)
	}
	byURL := make(map[string]string, len(arts))
	for _, a := range arts {
		byURL[a] = long
	}
	it := feed.Item{
		Kind: "tg_public", ID: "77", URL: "https://t.me/l/77", Title: "l-T1",
		Text: "список статей: " + arts[0] + " " + arts[1] + " " + arts[2],
	}
	e := &fakeLinkClassifier{}
	f := &fakeLinkSummarizer{byURL: byURL}
	lf := &fakeLinkFetcher{}
	poster := &fakePoster{}
	srcs := []TrackedSource{{Src: fakeSource{name: "l", items: []feed.Item{it}}, TopicID: 1}}
	if err := linkTestPipeline(st, &fakeClassifier{}, poster, srcs, e, f, lf, 5, 30).Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows, err := st.ListRunDigests(ctx, testNow.Unix())
	if err != nil {
		t.Fatalf("ListRunDigests: %v", err)
	}
	ditems, err := st.ListDigestItems(ctx, rows[0].ID)
	if err != nil {
		t.Fatalf("ListDigestItems: %v", err)
	}
	if len(ditems) != 1 {
		t.Fatalf("digest_items = %+v, want 1", ditems)
	}
	sum := ditems[0].Summary
	if n := utf8.RuneCountInString(sum); n > 600 {
		t.Errorf("summary = %d runes, want ≤600", n)
	}
	if !utf8.ValidString(sum) {
		t.Error("summary is not valid UTF-8 (обрезка не на границе руны)")
	}
	if !strings.HasPrefix(sum, "s1\n↳ "+long) {
		t.Errorf("summary lacks base + first ↳: %.60q", sum)
	}
	states, err := st.GetMessageStates(ctx, dedupKeys([]feed.Item{it}))
	if err != nil {
		t.Fatalf("GetMessageStates: %v", err)
	}
	if s := states[it.DedupKey()]; s.Summary != "s1" {
		t.Errorf("message_state summary = %q, want base s1 (без ↳)", s.Summary)
	}
}

// 9B кейс 6 (ревью F5, замена vacuous L3-golden): L2-golden с длинным
// ↳-саммари РОВНО 600 рун. Строка собирается strings.Repeat БЕЗ вызова
// truncateRunes — golden не должен дублировать логику pipeline. Существующие
// golden не меняются; drill_l2_links.golden — осознанное ДОБАВЛЕНИЕ
// (`go test ./internal/digest -run TestRenderDrillL2_LinkSummary_Golden -update`).
func TestRenderDrillL2_LinkSummary_Golden(t *testing.T) {
	const base = "Базовое саммари."       // 16 рун
	long := strings.Repeat("детали ", 83) // 7 × 83 = ровно 581 руна
	summary := base + "\n↳ " + long       // 16 + 3 + 581 = ровно 600 рун
	if n := utf8.RuneCountInString(summary); n != 600 {
		t.Fatalf("fixture broken: %d runes, want exactly 600", n)
	}
	item := storage.DigestItem{SourceID: 3, Title: "Новости Go", Summary: summary}
	res := renderDrillL2("Тема 1", []l2Section{
		{SourceID: 3, Name: "Habr", Items: []storage.DigestItem{item}},
	}, 42)
	if res == nil || res.Edit == nil {
		t.Fatal("renderDrillL2 = nil, want callback result")
	}
	compareGolden(t, "drill_l2_links", res.Edit.Text)
}

// 9B: повторный Run после успешной доставки — 0 fetch, 0 E/F
// (seen_items: прогон выходит до шага 4c).
func TestPipelineRun_RepeatAfterDeliveryNoLinkWork(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	const art = "https://art.test/rep"
	it := linkItem("r", "1", "r-T1", art)
	e := &fakeLinkClassifier{}
	f := &fakeLinkSummarizer{}
	lf := &fakeLinkFetcher{}
	srcs := []TrackedSource{{Src: fakeSource{name: "r", items: []feed.Item{it}}, TopicID: 1}}
	poster := &fakePoster{}
	if err := linkTestPipeline(st, &fakeClassifier{}, poster, srcs, e, f, lf, 5, 30).Run(ctx); err != nil {
		t.Fatalf("Run(1): %v", err)
	}
	if lf.callCount() != 1 || e.callCount() != 1 || f.callCount() != 1 {
		t.Fatalf("run1 counters fetch/E/F = %d/%d/%d, want 1/1/1",
			lf.callCount(), e.callCount(), f.callCount())
	}
	poster2 := &fakePoster{}
	p2 := linkTestPipeline(st, &fakeClassifier{}, poster2, srcs, e, f, lf, 5, 30)
	p2.Now = func() time.Time { return testNow.Add(time.Hour) }
	if err := p2.Run(ctx); err != nil {
		t.Fatalf("Run(2): %v", err)
	}
	if poster2.calls() != 0 {
		t.Errorf("run2 delivered again: calls = %d, want 0", poster2.calls())
	}
	if lf.callCount() != 1 || e.callCount() != 1 || f.callCount() != 1 {
		t.Errorf("counters after run2 = %d/%d/%d, want still 1/1/1",
			lf.callCount(), e.callCount(), f.callCount())
	}
}

// --- Подэтап 9B, ре-ревью: F1 (политика кеша course/ad), F3 (SSRF-guard),
// F9a (кросс-топик бюджет), F9b (сбой доставки + ссылки). ---

// F1-i: E вернул course на ссылку внутри NEWS-сообщения (LINK-level, в
// отличие от message-level skip в TestPipelineRun_CourseAndAdMessageSkipsLinkStep)
// → fetch НЕ вызван, строка link_cache{kind=course, title=”, summary=”}
// создана, дайджест цел без ↳.
func TestPipelineRun_CourseLinkKindCachedWithoutFetch(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	const art = "https://course-link.test/x"
	it := linkItem("cl", "1", "cl-T1", art)
	e := &fakeLinkClassifier{byURL: map[string]string{art: llm.LinkKindCourse}}
	f := &fakeLinkSummarizer{}
	lf := &fakeLinkFetcher{}
	poster := &fakePoster{}
	srcs := []TrackedSource{{Src: fakeSource{name: "cl", items: []feed.Item{it}}, TopicID: 1}}
	if err := linkTestPipeline(st, &fakeClassifier{}, poster, srcs, e, f, lf, 5, 30).Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if lf.callCount() != 0 || f.callCount() != 0 {
		t.Errorf("fetch/F calls = %d/%d, want 0/0 (course не фетчится)", lf.callCount(), f.callCount())
	}
	cache, err := st.GetLinkCache(ctx, []string{art})
	if err != nil {
		t.Fatalf("GetLinkCache: %v", err)
	}
	if len(cache) != 1 {
		t.Fatalf("link_cache = %+v, want 1 row", cache)
	}
	if row := cache[art]; row.Kind != llm.LinkKindCourse || row.Title != "" || row.Summary != "" {
		t.Errorf("cache row = %+v, want course + empty title/summary", row)
	}
	links, err := st.ListItemLinks(ctx, it.DedupKey())
	if err != nil {
		t.Fatalf("ListItemLinks: %v", err)
	}
	if len(links) != 1 || links[0].Kind != llm.LinkKindCourse {
		t.Errorf("item_links = %+v, want one course", links)
	}
	rows, err := st.ListRunDigests(ctx, testNow.Unix())
	if err != nil {
		t.Fatalf("ListRunDigests: %v", err)
	}
	ditems, err := st.ListDigestItems(ctx, rows[0].ID)
	if err != nil {
		t.Fatalf("ListDigestItems: %v", err)
	}
	if len(ditems) != 1 || strings.Contains(ditems[0].Summary, "↳") {
		t.Errorf("digest_items = %+v, want base summary without ↳", ditems)
	}
}

// F1-ii: pre-seed богатой строки {article,title,summary} + текущий E вернул ad
// на тот же URL → строка НЕ изменена (summary цел), fetch не вызван, без ↳.
func TestPipelineRun_AdLinkKindDoesNotClobberRichCache(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	const art = "https://rich.test/x"
	if err := st.UpsertLinkCache(ctx, []storage.LinkCacheRow{
		{URL: art, Kind: llm.LinkKindArticle, Title: "старая", Summary: "кешированное саммари"},
	}); err != nil {
		t.Fatalf("UpsertLinkCache: %v", err)
	}
	it := linkItem("rc", "1", "rc-T1", art)
	e := &fakeLinkClassifier{byURL: map[string]string{art: llm.LinkKindAd}}
	f := &fakeLinkSummarizer{}
	lf := &fakeLinkFetcher{}
	poster := &fakePoster{}
	srcs := []TrackedSource{{Src: fakeSource{name: "rc", items: []feed.Item{it}}, TopicID: 1}}
	if err := linkTestPipeline(st, &fakeClassifier{}, poster, srcs, e, f, lf, 5, 30).Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if lf.callCount() != 0 || f.callCount() != 0 {
		t.Errorf("fetch/F calls = %d/%d, want 0/0", lf.callCount(), f.callCount())
	}
	cache, err := st.GetLinkCache(ctx, []string{art})
	if err != nil {
		t.Fatalf("GetLinkCache: %v", err)
	}
	if row := cache[art]; row.Kind != llm.LinkKindArticle || row.Title != "старая" ||
		row.Summary != "кешированное саммари" {
		t.Errorf("cache row = %+v, want UNCHANGED rich article row", row)
	}
	rows, err := st.ListRunDigests(ctx, testNow.Unix())
	if err != nil {
		t.Fatalf("ListRunDigests: %v", err)
	}
	ditems, err := st.ListDigestItems(ctx, rows[0].ID)
	if err != nil {
		t.Fatalf("ListDigestItems: %v", err)
	}
	if len(ditems) != 1 || strings.Contains(ditems[0].Summary, "↳") {
		t.Errorf("digest_items = %+v, want no ↳ (текущий kind=ad)", ditems)
	}
}

// F1-iii: внутрипрогонный конфликт видов одного URL в одной теме — msg1
// X=article, msg2 X=ad → Fetcher вызван ОДИН раз, ad-строка не пишется,
// строка article c title, ↳ только у msg1.
func TestPipelineRun_ConflictingLinkKindsFetchOnce(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	const x = "https://shared.test/x"
	it1 := linkItem("c1", "1", "c1-T1", x)
	it2 := linkItem("c2", "2", "c2-T2", x)
	e := &fakeLinkClassifier{byPair: map[[2]int]string{{2, 1}: llm.LinkKindAd}}
	f := &fakeLinkSummarizer{}
	lf := &fakeLinkFetcher{}
	poster := &fakePoster{}
	srcs := []TrackedSource{
		{Src: fakeSource{name: "c1", items: []feed.Item{it1}}, SourceID: 1, TopicID: 1},
		{Src: fakeSource{name: "c2", items: []feed.Item{it2}}, SourceID: 2, TopicID: 1},
	}
	if err := linkTestPipeline(st, &fakeClassifier{}, poster, srcs, e, f, lf, 5, 30).Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if lf.callCount() != 1 {
		t.Fatalf("fetch calls = %d, want 1 (X фетчится один раз)", lf.callCount())
	}
	cache, err := st.GetLinkCache(ctx, []string{x})
	if err != nil {
		t.Fatalf("GetLinkCache: %v", err)
	}
	if len(cache) != 1 {
		t.Fatalf("link_cache = %+v, want exactly 1 row", cache)
	}
	if row := cache[x]; row.Kind != llm.LinkKindArticle || row.Title != "title "+x ||
		row.Summary != "сжато: "+x {
		t.Errorf("cache row = %+v, want article + title + summary", row)
	}
	rows, err := st.ListRunDigests(ctx, testNow.Unix())
	if err != nil {
		t.Fatalf("ListRunDigests: %v", err)
	}
	ditems, err := st.ListDigestItems(ctx, rows[0].ID)
	if err != nil {
		t.Fatalf("ListDigestItems: %v", err)
	}
	if len(ditems) != 2 {
		t.Fatalf("digest_items = %+v, want 2", ditems)
	}
	for _, di := range ditems {
		has := strings.Contains(di.Summary, "↳")
		switch di.Title {
		case "c1-T1":
			if !has {
				t.Errorf("msg1 (article) lacks ↳: %q", di.Summary)
			}
		case "c2-T2":
			if has {
				t.Errorf("msg2 (ad) got ↳: %q", di.Summary)
			}
		}
	}
	l1, err := st.ListItemLinks(ctx, it1.DedupKey())
	if err != nil {
		t.Fatalf("ListItemLinks(it1): %v", err)
	}
	l2, err := st.ListItemLinks(ctx, it2.DedupKey())
	if err != nil {
		t.Fatalf("ListItemLinks(it2): %v", err)
	}
	if len(l1) != 1 || l1[0].Kind != llm.LinkKindArticle {
		t.Errorf("item_links(it1) = %+v, want article", l1)
	}
	if len(l2) != 1 || l2[0].Kind != llm.LinkKindAd {
		t.Errorf("item_links(it2) = %+v, want ad", l2)
	}
}

// F1-iv: кеш-строка {course} прошлых прогонов + текущий E=article → строка
// курса = ПРОМАХ: fetch выполнен, строка апгрейднута до article с title и
// summary из F.
func TestPipelineRun_CachedCourseUpgradedByFetch(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	const art = "https://upg.test/x"
	if err := st.UpsertLinkCache(ctx, []storage.LinkCacheRow{
		{URL: art, Kind: llm.LinkKindCourse},
	}); err != nil {
		t.Fatalf("UpsertLinkCache: %v", err)
	}
	it := linkItem("u", "1", "u-T1", art)
	e := &fakeLinkClassifier{} // дефолт article
	f := &fakeLinkSummarizer{}
	lf := &fakeLinkFetcher{}
	poster := &fakePoster{}
	srcs := []TrackedSource{{Src: fakeSource{name: "u", items: []feed.Item{it}}, TopicID: 1}}
	if err := linkTestPipeline(st, &fakeClassifier{}, poster, srcs, e, f, lf, 5, 30).Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if lf.callCount() != 1 || f.callCount() != 1 {
		t.Fatalf("fetch/F calls = %d/%d, want 1/1 (course-строка = промах)", lf.callCount(), f.callCount())
	}
	cache, err := st.GetLinkCache(ctx, []string{art})
	if err != nil {
		t.Fatalf("GetLinkCache: %v", err)
	}
	if row := cache[art]; row.Kind != llm.LinkKindArticle || row.Title != "title "+art ||
		row.Summary != "сжато: "+art {
		t.Errorf("cache row = %+v, want upgraded article + title + F summary", row)
	}
	rows, err := st.ListRunDigests(ctx, testNow.Unix())
	if err != nil {
		t.Fatalf("ListRunDigests: %v", err)
	}
	ditems, err := st.ListDigestItems(ctx, rows[0].ID)
	if err != nil {
		t.Fatalf("ListDigestItems: %v", err)
	}
	if len(ditems) != 1 || ditems[0].Summary != "s1\n↳ сжато: "+art {
		t.Errorf("digest_items = %+v, want «s1\\n↳ сжато: %s»", ditems, art)
	}
}

// F3: SSRF-guard — literal-внутренние цели (loopback, cloud metadata, private,
// IPv6 loopback, localhost) НЕ фетчатся, БЕЗ траты бюджета (totalMax=1 уходит
// целиком на публичный URL) и БЕЗ строк link_cache; дайджест цел, Run=nil.
// Публичный кейс — hostname example.com с фейк-fetcher: реального HTTP нет,
// DNS либо резолвит публичный IP, либо (sandbox) падает → guard fail-open.
func TestPipelineRun_SSRFGuardSkipsInternalTargets(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	internal := []string{
		"http://127.0.0.1:9/x",
		"http://169.254.169.254/latest",
		"http://10.0.0.1/",
		"http://[::1]/x",
		"http://localhost/x",
	}
	const public = "https://example.com/pub"
	all := append(append([]string{}, internal...), public)
	items := make([]feed.Item, 0, len(all))
	for i, u := range all {
		items = append(items, linkItem("s", strconv.Itoa(i+1), "s-T"+strconv.Itoa(i+1), u))
	}
	e := &fakeLinkClassifier{}
	f := &fakeLinkSummarizer{}
	lf := &fakeLinkFetcher{}
	poster := &fakePoster{}
	srcs := []TrackedSource{{Src: fakeSource{name: "s", items: items}, TopicID: 1}}
	if err := linkTestPipeline(st, &fakeClassifier{}, poster, srcs, e, f, lf, 5, 1).Run(ctx); err != nil {
		t.Fatalf("Run = %v, want nil (guard — деградация без ошибки)", err)
	}
	if lf.callCount() != 1 {
		t.Fatalf("fetch calls = %d, want 1 (только публичный URL)", lf.callCount())
	}
	if len(lf.urls) != 1 || lf.urls[0] != public {
		t.Errorf("fetched urls = %v, want [%s]", lf.urls, public)
	}
	probe := []string{
		"http://127.0.0.1:9/x", "http://169.254.169.254/latest",
		"http://10.0.0.1", "http://10.0.0.1/", "http://[::1]/x",
		"http://localhost/x", public,
	}
	cache, err := st.GetLinkCache(ctx, probe)
	if err != nil {
		t.Fatalf("GetLinkCache: %v", err)
	}
	if len(cache) != 1 {
		t.Fatalf("link_cache = %+v, want exactly 1 row (public)", cache)
	}
	if row := cache[public]; row.Kind != llm.LinkKindArticle {
		t.Errorf("public row = %+v, want article", row)
	}
	rows, err := st.ListRunDigests(ctx, testNow.Unix())
	if err != nil {
		t.Fatalf("ListRunDigests: %v", err)
	}
	ditems, err := st.ListDigestItems(ctx, rows[0].ID)
	if err != nil {
		t.Fatalf("ListDigestItems: %v", err)
	}
	if len(ditems) != len(all) {
		t.Fatalf("digest_items = %+v, want %d (дайджест цел)", ditems, len(all))
	}
	for _, di := range ditems {
		has := strings.Contains(di.Summary, "↳")
		if want := di.Title == "s-T"+strconv.Itoa(len(all)); has != want {
			t.Errorf("item %s has↳ = %v, want %v", di.Title, has, want)
		}
	}
}

// F9a: кросс-топик — LinkFetchTotalMax=1; тема1 URL A (article) → fetch=1;
// тема2 тот же A → кеш-хит из БД (immediate-upsert темы1), fetch НЕ вызван, ↳
// получен; тема2 URL B → бюджет исчерпан: B в item_links, без строки кеша,
// без ↳.
func TestPipelineRun_CrossTopicCacheHitAndBudget(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	const a = "https://shared.test/a"
	const b = "https://second.test/b"
	it1 := linkItem("t1", "1", "t1-T1", a)
	it2 := linkItem("t2", "2", "t2-T1", a)
	it3 := linkItem("t2", "3", "t2-T2", b)
	e := &fakeLinkClassifier{}
	f := &fakeLinkSummarizer{}
	lf := &fakeLinkFetcher{}
	poster := &fakePoster{}
	srcs := []TrackedSource{
		{Src: fakeSource{name: "t1", items: []feed.Item{it1}}, SourceID: 1, TopicID: 1},
		{Src: fakeSource{name: "t2", items: []feed.Item{it2, it3}}, SourceID: 2, TopicID: 2},
	}
	if err := linkTestPipeline(st, &fakeClassifier{}, poster, srcs, e, f, lf, 5, 1).Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if lf.callCount() != 1 || e.callCount() != 2 {
		t.Fatalf("fetch/E calls = %d/%d, want 1/2", lf.callCount(), e.callCount())
	}
	cache, err := st.GetLinkCache(ctx, []string{a, b})
	if err != nil {
		t.Fatalf("GetLinkCache: %v", err)
	}
	if len(cache) != 1 {
		t.Fatalf("link_cache = %+v, want exactly 1 row (A)", cache)
	}
	if row := cache[a]; row.Kind != llm.LinkKindArticle || row.Title != "title "+a ||
		row.Summary != "сжато: "+a {
		t.Errorf("cache row A = %+v, want article + title + summary", row)
	}
	l2, err := st.ListItemLinks(ctx, it2.DedupKey())
	if err != nil {
		t.Fatalf("ListItemLinks(it2): %v", err)
	}
	l3, err := st.ListItemLinks(ctx, it3.DedupKey())
	if err != nil {
		t.Fatalf("ListItemLinks(it3): %v", err)
	}
	if len(l2) != 1 || l2[0].URL != a || l2[0].Kind != llm.LinkKindArticle {
		t.Errorf("item_links(it2) = %+v, want one article A", l2)
	}
	if len(l3) != 1 || l3[0].URL != b || l3[0].Kind != llm.LinkKindArticle {
		t.Errorf("item_links(it3) = %+v, want one article B (без строки кеша)", l3)
	}
	rows, err := st.ListRunDigests(ctx, testNow.Unix())
	if err != nil {
		t.Fatalf("ListRunDigests: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2 (две темы)", rows)
	}
	for _, r := range rows {
		ditems, err := st.ListDigestItems(ctx, r.ID)
		if err != nil {
			t.Fatalf("ListDigestItems: %v", err)
		}
		for _, di := range ditems {
			has := strings.Contains(di.Summary, "↳ сжато: "+a)
			switch di.Title {
			case "t1-T1", "t2-T1":
				if !has {
					t.Errorf("%s lacks ↳ (кеш-хит A): %q", di.Title, di.Summary)
				}
			case "t2-T2":
				if strings.Contains(di.Summary, "↳") {
					t.Errorf("t2-T2 got ↳ despite exhausted budget: %q", di.Summary)
				}
			}
		}
	}
}

// F9b: сбой доставки ПОСЛЕ шага 4c — run1: fetch=1, E=1, F=1, link_cache с
// summary; run2 (те же элементы, seen не помечен): E вызван ЗАНОВО (счётчик
// E=2 — документированное отклонение 9B-4 п.6), fetch НЕ растёт (кеш-хит),
// F не вызван (freshPages пуст), доставка успешна, ↳ на месте.
func TestPipelineRun_DeliveryFailureThenRetryUsesLinkCache(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	const art = "https://retry.test/x"
	it := linkItem("d", "1", "d-T1", art)
	e := &fakeLinkClassifier{}
	f := &fakeLinkSummarizer{}
	lf := &fakeLinkFetcher{}
	srcs := []TrackedSource{{Src: fakeSource{name: "d", items: []feed.Item{it}}, TopicID: 1}}

	poster1 := &fakePoster{err: errors.New("deliver boom")}
	p1 := linkTestPipeline(st, &fakeClassifier{}, poster1, srcs, e, f, lf, 5, 30)
	if err := p1.Run(ctx); err == nil {
		t.Fatal("Run(1) = nil, want delivery error (4c завершается ДО доставки)")
	}
	if lf.callCount() != 1 || e.callCount() != 1 || f.callCount() != 1 {
		t.Fatalf("run1 counters fetch/E/F = %d/%d/%d, want 1/1/1",
			lf.callCount(), e.callCount(), f.callCount())
	}
	cache, err := st.GetLinkCache(ctx, []string{art})
	if err != nil {
		t.Fatalf("GetLinkCache: %v", err)
	}
	if row := cache[art]; row.Summary != "сжато: "+art {
		t.Fatalf("run1 cache row = %+v, want summary saved before delivery", row)
	}

	poster2 := &fakePoster{}
	p2 := linkTestPipeline(st, &fakeClassifier{}, poster2, srcs, e, f, lf, 5, 30)
	p2.Now = func() time.Time { return testNow.Add(time.Hour) }
	if err := p2.Run(ctx); err != nil {
		t.Fatalf("Run(2): %v", err)
	}
	if e.callCount() != 2 {
		t.Errorf("E calls = %d, want 2 (документированное отклонение 9B-4 п.6)", e.callCount())
	}
	if lf.callCount() != 1 {
		t.Errorf("fetch calls = %d, want still 1 (кеш-хит — GET не повторяется)", lf.callCount())
	}
	if f.callCount() != 1 {
		t.Errorf("F calls = %d, want still 1 (freshPages пуст)", f.callCount())
	}
	if poster2.calls() != 1 {
		t.Fatalf("run2 poster calls = %d, want 1", poster2.calls())
	}
	rows, err := st.ListRunDigests(ctx, testNow.Add(time.Hour).Unix())
	if err != nil {
		t.Fatalf("ListRunDigests: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("run2 rows = %+v, want 1", rows)
	}
	ditems, err := st.ListDigestItems(ctx, rows[0].ID)
	if err != nil {
		t.Fatalf("ListDigestItems: %v", err)
	}
	if len(ditems) != 1 || ditems[0].Summary != "s1\n↳ сжато: "+art {
		t.Errorf("run2 digest_items = %+v, want «s1\\n↳ сжато: %s»", ditems, art)
	}
}
