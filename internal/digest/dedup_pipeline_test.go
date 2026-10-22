// Pipeline-тесты вызова B (Этап 9C, план §4.4, тест-кейс 3): реальный Run
// поверх временной SQLite + фейки A/B со счётчиками. Кейсы: кластер {1,2,3}
// primary=1 → 1 элемент в digest_items, 2 — dropped_dup+seen, cluster_id
// одинаковый '<runID>:1'; повторный Run после сбоя доставки → 0 новых вызовов
// B (кеш), после успешной доставки → 0 (seen); сбой B → деградация (все
// primary, Run=nil); ad исключён из входа B и не primary. Обратная
// совместимость Dedup=nil покрыта ВСЕМ существующим pipeline_test.go (9A/9B):
// nil-Dedup пропускает шаг 4a-bis целиком.

package digest

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
	"github.com/akomyagin/aiTelegaBot/internal/llm"
	"github.com/akomyagin/aiTelegaBot/internal/storage"
)

// fakeDeduper — фейк вызова B (llm.Deduper) со счётчиком вызовов и записью
// входов. clusters/err — настраиваемый результат.
type fakeDeduper struct {
	mu       sync.Mutex
	calls    int
	inputs   [][]llm.DedupItem
	clusters []llm.DupCluster
	err      error
}

func (f *fakeDeduper) FindDuplicates(ctx context.Context,
	items []llm.DedupItem) ([]llm.DupCluster, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	cp := make([]llm.DedupItem, len(items))
	copy(cp, items)
	f.inputs = append(f.inputs, cp)
	if f.err != nil {
		return nil, f.err
	}
	out := make([]llm.DupCluster, len(f.clusters))
	copy(out, f.clusters)
	return out, nil
}

func (f *fakeDeduper) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeDeduper) lastInput() []llm.DedupItem {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.inputs) == 0 {
		return nil
	}
	return f.inputs[len(f.inputs)-1]
}

// dedupNewsItems — 3 переписанных новости из 3 источников (порядок collect).
func dedupNewsItems() []feed.Item {
	base := time.Date(2026, 10, 19, 6, 0, 0, 0, time.UTC)
	return []feed.Item{
		{URL: "https://a.test/n/1", Title: "n-T1", Text: "Переписанная новость раз",
			Source: "A", Published: base.Add(2 * time.Hour)},
		{URL: "https://b.test/n/2", Title: "n-T2", Text: "Переписанная новость два",
			Source: "B", Published: base},
		{URL: "https://c.test/n/3", Title: "n-T3", Text: "Переписанная новость три",
			Source: "C", Published: base.Add(time.Hour)},
	}
}

// dedupSources — по одному fakeSource на элемент, SourceID 1..n, TopicID 0
// («Без темы» → 1): SourceID попадают в stamped-элементы и в B-вход.
func dedupSources(items []feed.Item) []TrackedSource {
	srcs := make([]TrackedSource, 0, len(items))
	for i, it := range items {
		srcs = append(srcs, TrackedSource{
			Src:      fakeSource{name: it.Source, items: []feed.Item{it}},
			SourceID: int64(i + 1),
			TopicID:  0,
		})
	}
	return srcs
}

func testClusterID(n int) string {
	return strconv.FormatInt(testNow.Unix(), 10) + ":" + strconv.Itoa(n)
}

// TestPipelineRun_DedupClusterPrimaryOnly — кейс 3 плана: кластер {1,2,3},
// primary=1 → в digest_items 1 элемент; дубли — dropped_dup + seen;
// cluster_id у всех троих одинаковый '<runID>:1'.
func TestPipelineRun_DedupClusterPrimaryOnly(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	items := dedupNewsItems()
	cls := &fakeClassifier{}
	dd := &fakeDeduper{clusters: []llm.DupCluster{{Indices: []int{1, 2, 3}, Primary: 1}}}
	p := newTestPipeline(st, cls, &fakePoster{}, dedupSources(items), testNow)
	p.Dedup = dd

	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := dd.callCount(); n != 1 {
		t.Fatalf("B calls = %d, want 1", n)
	}
	// B-вход: все 3 элемента в порядке collect, SourceID проставлены.
	in := dd.lastInput()
	if len(in) != 3 || in[0].Title != "n-T1" || in[2].Title != "n-T3" {
		t.Fatalf("B input = %+v, want n-T1..n-T3 in collect order", in)
	}
	for i, want := range []int64{1, 2, 3} {
		if in[i].SourceID != want {
			t.Errorf("B input[%d].SourceID = %d, want %d", i, in[i].SourceID, want)
		}
	}

	rows, err := st.ListRunDigests(ctx, testNow.Unix())
	if err != nil {
		t.Fatalf("ListRunDigests: %v", err)
	}
	if len(rows) != 1 || rows[0].ItemCount != 1 {
		t.Fatalf("rows = %+v, want a single topic row with ItemCount=1", rows)
	}
	ditems, err := st.ListDigestItems(ctx, rows[0].ID)
	if err != nil {
		t.Fatalf("ListDigestItems: %v", err)
	}
	if len(ditems) != 1 || ditems[0].Title != "n-T1" {
		t.Fatalf("digest items = %+v, want only the primary n-T1", ditems)
	}

	states, err := st.GetMessageStates(ctx, dedupKeys(items))
	if err != nil {
		t.Fatalf("GetMessageStates: %v", err)
	}
	cid := testClusterID(1)
	if s := states[items[0].DedupKey()]; s.Status != stateDone || !s.IsPrimary || s.ClusterID != cid {
		t.Errorf("primary state = %+v, want done/primary/%s", s, cid)
	}
	for _, i := range []int{1, 2} {
		s := states[items[i].DedupKey()]
		if s.Status != stateDroppedDup || s.IsPrimary || s.ClusterID != cid {
			t.Errorf("dup state[%d] = %+v, want dropped_dup/non-primary/%s", i, s, cid)
		}
	}

	fresh, err := st.FilterUnseen(ctx, items)
	if err != nil {
		t.Fatalf("FilterUnseen: %v", err)
	}
	if len(fresh) != 0 {
		t.Errorf("fresh = %d, want 0 (primary доставлен, дубли seen до доставки)", len(fresh))
	}
}

// TestPipelineRun_DedupNotRecalledOnCached — повторный Run после СБОЯ
// доставки: B не зовётся по закешированным (счётчик не растёт), доставка из
// кеша; после УСПЕШНОЙ доставки третий Run тоже делает 0 вызовов B (seen).
func TestPipelineRun_DedupNotRecalledOnCached(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	items := dedupNewsItems()
	cls := &fakeClassifier{}
	dd := &fakeDeduper{clusters: []llm.DupCluster{{Indices: []int{1, 2, 3}, Primary: 1}}}
	poster := &fakePoster{err: errors.New("network down")}
	p := newTestPipeline(st, cls, poster, dedupSources(items), testNow)
	p.Dedup = dd

	if err := p.Run(ctx); err == nil {
		t.Fatal("Run #1 succeeded, want delivery error")
	}
	if n := dd.callCount(); n != 1 {
		t.Fatalf("B calls after run #1 = %d, want 1", n)
	}
	// G7: seen дублей (не-degraded кластер) делается ДО доставки — TOC не
	// доехал, но FilterUnseen по dup-ключам уже пуст.
	dupFresh, err := st.FilterUnseen(ctx, items[1:])
	if err != nil {
		t.Fatalf("FilterUnseen: %v", err)
	}
	if len(dupFresh) != 0 {
		t.Errorf("dups unseen after failed delivery: %+v (seen должен быть до доставки)", dupFresh)
	}

	// Повтор: дубли уже seen (первый прогон), primary — в кеше done; uncls
	// пуст → B не вызывается, дайджест пересобирается из кеша и доставляется.
	poster.err = nil
	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run #2: %v", err)
	}
	if n := dd.callCount(); n != 1 {
		t.Fatalf("B calls after run #2 = %d, want still 1 (0 вызовов по закешированным)", n)
	}
	if n := cls.callCount(); n != 1 {
		t.Fatalf("A calls after run #2 = %d, want still 1 (кеш message_state)", n)
	}
	rows, err := st.ListRunDigests(ctx, testNow.Unix())
	if err != nil {
		t.Fatalf("ListRunDigests: %v", err)
	}
	var deliveredRow int64 = -1
	for _, r := range rows {
		if r.Delivered {
			deliveredRow = r.ID
		}
	}
	if deliveredRow < 0 {
		t.Fatalf("rows = %+v, want one delivered row", rows)
	}
	ditems, err := st.ListDigestItems(ctx, deliveredRow)
	if err != nil {
		t.Fatalf("ListDigestItems: %v", err)
	}
	if len(ditems) != 1 || ditems[0].Title != "n-T1" {
		t.Fatalf("digest items = %+v, want only n-T1 from cache", ditems)
	}

	// После успешной доставки: всё seen → Run #3 выходит тихо, 0 вызовов B.
	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run #3: %v", err)
	}
	if n := dd.callCount(); n != 1 {
		t.Fatalf("B calls after run #3 = %d, want still 1 (seen)", n)
	}
}

// TestPipelineRun_DedupErrorDegrades — сбой B (транспорт/двойной мусор):
// все элементы primary, тема НЕ пропускается, Run=nil, cluster_id=” в states.
func TestPipelineRun_DedupErrorDegrades(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	items := dedupNewsItems()
	dd := &fakeDeduper{err: errors.New("dedup down")}
	p := newTestPipeline(st, &fakeClassifier{}, &fakePoster{}, dedupSources(items), testNow)
	p.Dedup = dd

	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run = %v, want nil (ошибка B — деградация, не сбой темы)", err)
	}
	rows, err := st.ListRunDigests(ctx, testNow.Unix())
	if err != nil {
		t.Fatalf("ListRunDigests: %v", err)
	}
	if len(rows) != 1 || rows[0].ItemCount != 3 {
		t.Fatalf("rows = %+v, want a single row with all 3 items", rows)
	}
	ditems, err := st.ListDigestItems(ctx, rows[0].ID)
	if err != nil {
		t.Fatalf("ListDigestItems: %v", err)
	}
	if len(ditems) != 3 {
		t.Fatalf("digest items = %d, want 3 (деградация: дедуп пропущен)", len(ditems))
	}
	states, err := st.GetMessageStates(ctx, dedupKeys(items))
	if err != nil {
		t.Fatalf("GetMessageStates: %v", err)
	}
	for i, it := range items {
		s := states[it.DedupKey()]
		if s.ClusterID != "" || !s.IsPrimary || s.Status != stateDone {
			t.Errorf("state[%d] = %+v, want done/primary/cluster_id=''", i, s)
		}
	}
}

// TestPipelineRun_DedupAdExcludedFromInput — ad-элемент среди похожих:
// исключён из входа B и не primary в дайджесте (dropped_ad + seen).
func TestPipelineRun_DedupAdExcludedFromInput(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	items := dedupNewsItems()
	items = append(items, feed.Item{URL: "https://d.test/n/4", Title: "n-T4",
		Text: "Переписанная новость четыре", Source: "D",
		Published: time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)})
	cls := &fakeClassifier{byTitle: map[string]fakeOutcome{
		"n-T2": {class: llm.ClassAd},
	}}
	// Индексы B — в пространстве входа B (n-T1, n-T3, n-T4): {1,2} → n-T1/n-T3.
	dd := &fakeDeduper{clusters: []llm.DupCluster{{Indices: []int{1, 2}, Primary: 1}}}
	p := newTestPipeline(st, cls, &fakePoster{}, dedupSources(items), testNow)
	p.Dedup = dd

	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	in := dd.lastInput()
	if len(in) != 3 {
		t.Fatalf("B input = %+v, want 3 items (ad исключён)", in)
	}
	for i, want := range []string{"n-T1", "n-T3", "n-T4"} {
		if in[i].Title != want {
			t.Fatalf("B input[%d].Title = %q, want %q (порядок collect без ad)", i, in[i].Title, want)
		}
	}
	rows, err := st.ListRunDigests(ctx, testNow.Unix())
	if err != nil {
		t.Fatalf("ListRunDigests: %v", err)
	}
	if len(rows) != 1 || rows[0].ItemCount != 2 {
		t.Fatalf("rows = %+v, want ItemCount=2 (n-T1 + n-T4)", rows)
	}
	ditems, err := st.ListDigestItems(ctx, rows[0].ID)
	if err != nil {
		t.Fatalf("ListDigestItems: %v", err)
	}
	if len(ditems) != 2 || ditems[0].Title != "n-T1" || ditems[1].Title != "n-T4" {
		t.Fatalf("digest items = %+v, want n-T1,n-T4 (ad и dup не попадают)", ditems)
	}
	states, err := st.GetMessageStates(ctx, dedupKeys(items))
	if err != nil {
		t.Fatalf("GetMessageStates: %v", err)
	}
	if s := states[items[1].DedupKey()]; s.Status != stateDroppedAd {
		t.Errorf("ad state = %+v, want dropped_ad", s)
	}
	cid := testClusterID(1)
	if s := states[items[2].DedupKey()]; s.Status != stateDroppedDup || s.ClusterID != cid {
		t.Errorf("dup state = %+v, want dropped_dup/%s", s, cid)
	}
	if s := states[items[3].DedupKey()]; s.ClusterID != "" || !s.IsPrimary {
		t.Errorf("out-of-cluster state = %+v, want primary/cluster_id=''", s)
	}
	fresh, err := st.FilterUnseen(ctx, items)
	if err != nil {
		t.Fatalf("FilterUnseen: %v", err)
	}
	if len(fresh) != 0 {
		t.Errorf("fresh = %d, want 0 (ad и dup seen сразу, primary доставлен)", len(fresh))
	}
}

// --- фикс 9C-ревью №1: degraded-кластеры не персистятся и не seen ---

// TestPipelineRun_DedupDegradedClusterNotPersisted — degraded primary +
// не-degraded дубли: in-run дедуп работает (в digest_items только primary), но
// строки B-батча НЕ пишутся (дубли остаются done/is_primary=1/cluster_id=” от
// A-батча) и дубли НЕ seen — следующий прогон пересмотрит.
func TestPipelineRun_DedupDegradedClusterNotPersisted(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	items := dedupNewsItems()
	cls := &fakeClassifier{byTitle: map[string]fakeOutcome{
		"n-T1": {degraded: true}, // primary кластера деградирует
	}}
	dd := &fakeDeduper{clusters: []llm.DupCluster{{Indices: []int{1, 2, 3}, Primary: 1}}}
	p := newTestPipeline(st, cls, &fakePoster{}, dedupSources(items), testNow)
	p.Dedup = dd

	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows, err := st.ListRunDigests(ctx, testNow.Unix())
	if err != nil {
		t.Fatalf("ListRunDigests: %v", err)
	}
	if len(rows) != 1 || rows[0].ItemCount != 1 {
		t.Fatalf("rows = %+v, want a single row with ItemCount=1 (in-run дедуп применён)", rows)
	}
	ditems, err := st.ListDigestItems(ctx, rows[0].ID)
	if err != nil {
		t.Fatalf("ListDigestItems: %v", err)
	}
	if len(ditems) != 1 || ditems[0].Title != "n-T1" {
		t.Fatalf("digest items = %+v, want only the degraded primary n-T1", ditems)
	}
	states, err := st.GetMessageStates(ctx, dedupKeys(items))
	if err != nil {
		t.Fatalf("GetMessageStates: %v", err)
	}
	if len(states) != 2 {
		t.Fatalf("states = %+v, want 2 (degraded primary не кэшируется, B-батч пуст)", states)
	}
	for _, i := range []int{1, 2} {
		s := states[items[i].DedupKey()]
		if s.Status != stateDone || !s.IsPrimary || s.ClusterID != "" {
			t.Errorf("dup state[%d] = %+v, want done/primary/cluster_id='' (НЕ dropped_dup)", i, s)
		}
	}
	// Дубли НЕ seen: FilterUnseen возвращает их следующему прогону.
	fresh, err := st.FilterUnseen(ctx, items[1:])
	if err != nil {
		t.Fatalf("FilterUnseen: %v", err)
	}
	if len(fresh) != 2 {
		t.Errorf("fresh = %d, want 2 (дубли degraded-кластера остаются unseen)", len(fresh))
	}
}

// TestPipelineRun_OfflineDedupDegradedNoStates — offline-путь (Классификация и
// дедуп через llm.NewOffline): два элемента с одинаковым заголовком → в
// digest_items один; message_state НЕ пишется вообще (9A: деградация не
// кэшируется; 9C: degraded-кластер не персистится); дубль НЕ seen.
func TestPipelineRun_OfflineDedupDegradedNoStates(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	items := []feed.Item{
		{URL: "https://a.test/o/1", Title: "Go 1.24 Released!", Text: "Релиз Go", Source: "A"},
		{URL: "https://b.test/o/2", Title: "go 1.24   released", Text: "Переписано", Source: "B"},
	}
	off := llm.NewOffline()
	p := newTestPipeline(st, off, &fakePoster{}, dedupSources(items), testNow)
	p.Dedup = off

	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows, err := st.ListRunDigests(ctx, testNow.Unix())
	if err != nil {
		t.Fatalf("ListRunDigests: %v", err)
	}
	if len(rows) != 1 || rows[0].ItemCount != 1 {
		t.Fatalf("rows = %+v, want a single row with ItemCount=1", rows)
	}
	ditems, err := st.ListDigestItems(ctx, rows[0].ID)
	if err != nil {
		t.Fatalf("ListDigestItems: %v", err)
	}
	if len(ditems) != 1 || ditems[0].Title != "Go 1.24 Released!" {
		t.Fatalf("digest items = %+v, want only the primary", ditems)
	}
	states, err := st.GetMessageStates(ctx, dedupKeys(items))
	if err != nil {
		t.Fatalf("GetMessageStates: %v", err)
	}
	if len(states) != 0 {
		t.Errorf("states = %+v, want empty (offline-деградации не персистятся: 9A+9C)", states)
	}
	fresh, err := st.FilterUnseen(ctx, items[1:])
	if err != nil {
		t.Fatalf("FilterUnseen: %v", err)
	}
	if len(fresh) != 1 {
		t.Errorf("fresh = %d, want 1 (дубль не seen, primary доставлен)", len(fresh))
	}
}

// --- фикс 9C-ревью №2: L4-guard контракта Deduper ---

// TestPipelineRun_DedupInvalidClusterDropped — фейк возвращает невалидный
// кластер {1,99} (индекс вне 1..n) и валидный {2,3}: первый отброшен ЦЕЛИКОМ
// (без panic), второй применён.
func TestPipelineRun_DedupInvalidClusterDropped(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	items := dedupNewsItems()
	dd := &fakeDeduper{clusters: []llm.DupCluster{
		{Indices: []int{1, 99}, Primary: 1}, // 99 вне 1..3 → guard отбрасывает
		{Indices: []int{2, 3}, Primary: 2},  // валиден → применён
	}}
	p := newTestPipeline(st, &fakeClassifier{}, &fakePoster{}, dedupSources(items), testNow)
	p.Dedup = dd

	if err := p.Run(ctx); err != nil { // без guard — panic index out of range
		t.Fatalf("Run: %v", err)
	}
	rows, err := st.ListRunDigests(ctx, testNow.Unix())
	if err != nil {
		t.Fatalf("ListRunDigests: %v", err)
	}
	if len(rows) != 1 || rows[0].ItemCount != 2 {
		t.Fatalf("rows = %+v, want ItemCount=2 (n-T1 вне кластеров + n-T2 primary)", rows)
	}
	ditems, err := st.ListDigestItems(ctx, rows[0].ID)
	if err != nil {
		t.Fatalf("ListDigestItems: %v", err)
	}
	if len(ditems) != 2 || ditems[0].Title != "n-T1" || ditems[1].Title != "n-T2" {
		t.Fatalf("digest items = %+v, want n-T1,n-T2", ditems)
	}
	states, err := st.GetMessageStates(ctx, dedupKeys(items))
	if err != nil {
		t.Fatalf("GetMessageStates: %v", err)
	}
	cid := testClusterID(1) // нумерация — по валидированному списку кластеров
	if s := states[items[0].DedupKey()]; s.ClusterID != "" || !s.IsPrimary || s.Status != stateDone {
		t.Errorf("out-of-cluster state = %+v, want done/primary/cluster_id=''", s)
	}
	if s := states[items[1].DedupKey()]; s.ClusterID != cid || !s.IsPrimary || s.Status != stateDone {
		t.Errorf("primary state = %+v, want done/primary/%s", s, cid)
	}
	if s := states[items[2].DedupKey()]; s.ClusterID != cid || s.IsPrimary || s.Status != stateDroppedDup {
		t.Errorf("dup state = %+v, want dropped_dup/non-primary/%s", s, cid)
	}
	fresh, err := st.FilterUnseen(ctx, items)
	if err != nil {
		t.Fatalf("FilterUnseen: %v", err)
	}
	if len(fresh) != 0 {
		t.Errorf("fresh = %d, want 0", len(fresh))
	}
}

// --- фикс 9C-ревью №3: chooseDupPrimary, zero Published = «позднейшая» ---

func TestChooseDupPrimary(t *testing.T) {
	base := time.Date(2026, 10, 19, 6, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		items   []llm.DedupItem
		indices []int
		want    int
	}{
		{"earliest wins", []llm.DedupItem{
			{Published: base.Add(time.Hour), SourceID: 1, Text: "одинаковый текст"},
			{Published: base, SourceID: 2, Text: "одинаковый текст"},
			{Published: base.Add(2 * time.Hour), SourceID: 3, Text: "одинаковый текст"},
		}, []int{1, 2, 3}, 2},
		{"equal dates → longer text", []llm.DedupItem{
			{Published: base, Text: "короткий", SourceID: 1},
			{Published: base, Text: "более длинный текст", SourceID: 2},
		}, []int{1, 2}, 2},
		{"equal dates and text → smaller SourceID", []llm.DedupItem{
			{Published: base, Text: "x", SourceID: 5},
			{Published: base, Text: "y", SourceID: 3},
		}, []int{1, 2}, 2},
		{"zero дата проигрывает датированной (zero первой)", []llm.DedupItem{
			{SourceID: 1, Text: "без даты"},
			{Published: base, SourceID: 2, Text: "датированная"},
		}, []int{1, 2}, 2},
		{"zero дата проигрывает датированной (zero второй)", []llm.DedupItem{
			{Published: base, SourceID: 9, Text: "датированная"},
			{SourceID: 1, Text: "без даты"},
		}, []int{1, 2}, 1},
		{"все zero → длиннее текст", []llm.DedupItem{
			{SourceID: 1, Text: "а"},
			{SourceID: 2, Text: "аааа"},
		}, []int{1, 2}, 2},
		{"все zero, равный текст → меньший SourceID", []llm.DedupItem{
			{SourceID: 9, Text: "a"},
			{SourceID: 4, Text: "b"},
		}, []int{1, 2}, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := chooseDupPrimary(c.items, c.indices); got != c.want {
				t.Errorf("chooseDupPrimary = %d, want %d", got, c.want)
			}
		})
	}
}

// --- G1: сбой B-батча UpsertMessageState → откат in-run дедупа ЦЕЛИКОМ ---

// failSecondUpsertStore — второй вызов UpsertMessageState (B-батч) падает,
// первый (A-батч) успешен; записывает ключи MarkSeenBatch. failStoreOps-по-ключу
// не годится: A и B пишут одни и те же ключи — только счётчик вызовов.
type failSecondUpsertStore struct {
	storage.Store
	mu       sync.Mutex
	upserts  int
	seenKeys []string
}

func (s *failSecondUpsertStore) UpsertMessageState(ctx context.Context,
	states []storage.MessageState) error {
	s.mu.Lock()
	s.upserts++
	n := s.upserts
	s.mu.Unlock()
	if n == 2 {
		return errors.New("upsert B boom")
	}
	return s.Store.UpsertMessageState(ctx, states)
}

func (s *failSecondUpsertStore) MarkSeenBatch(ctx context.Context, keys []string) error {
	s.mu.Lock()
	s.seenKeys = append(s.seenKeys, keys...)
	s.mu.Unlock()
	return s.Store.MarkSeenBatch(ctx, keys)
}

func (s *failSecondUpsertStore) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seenKeys...)
}

func TestPipelineRun_DedupUpsertFailureRollsBack(t *testing.T) {
	base := openTestStore(t)
	fss := &failSecondUpsertStore{Store: base}
	ctx := context.Background()
	items := dedupNewsItems()
	dd := &fakeDeduper{clusters: []llm.DupCluster{{Indices: []int{1, 2, 3}, Primary: 1}}}
	p := newTestPipeline(fss, &fakeClassifier{}, &fakePoster{}, dedupSources(items), testNow)
	p.Dedup = dd

	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run = %v, want nil (сбой B-upsert — деградация, не сбой темы)", err)
	}
	fss.mu.Lock()
	upserts := fss.upserts
	fss.mu.Unlock()
	if upserts != 2 {
		t.Fatalf("UpsertMessageState calls = %d, want 2 (A успешен, B упал)", upserts)
	}
	rows, err := base.ListRunDigests(ctx, testNow.Unix())
	if err != nil {
		t.Fatalf("ListRunDigests: %v", err)
	}
	if len(rows) != 1 || rows[0].ItemCount != 3 {
		t.Fatalf("rows = %+v, want a single row with all 3 items (откат: все primary)", rows)
	}
	ditems, err := base.ListDigestItems(ctx, rows[0].ID)
	if err != nil {
		t.Fatalf("ListDigestItems: %v", err)
	}
	if len(ditems) != 3 {
		t.Fatalf("digest items = %d, want 3 (in-run дедуп откачен)", len(ditems))
	}
	states, err := base.GetMessageStates(ctx, dedupKeys(items))
	if err != nil {
		t.Fatalf("GetMessageStates: %v", err)
	}
	for i, it := range items {
		s := states[it.DedupKey()]
		if s.Status != stateDone || !s.IsPrimary || s.ClusterID != "" {
			t.Errorf("state[%d] = %+v, want done/primary/cluster_id='' (только A-батч)", i, s)
		}
	}
	// Дубли НЕ seen: MarkSeenBatch не вызывался вовсе (ads нет, dupDropped откачен).
	if keys := fss.seen(); len(keys) != 0 {
		t.Errorf("MarkSeenBatch keys = %v, want none (откат — дубли не seen)", keys)
	}
}

// --- 6c: кросс-топик дубль dedup_key — из кеша dropped_dup, без B ---

func TestPipelineRun_DedupCrossTopicCachedDroppedDup(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	fiveID, err := st.CreateTopic(ctx, "Five")
	if err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	const dupURL = "https://dup.test/d"
	t1a := feed.Item{URL: "https://a.test/1", Title: "t1-A", Text: "Событие, часть 1", Source: "A"}
	t1b := feed.Item{URL: dupURL, Title: "t1-B", Text: "Событие, часть 2", Source: "B"}
	t2a := feed.Item{URL: dupURL, Title: "t2-A", Text: "Тот же URL, тема Five", Source: "C"}
	t2b := feed.Item{URL: "https://d.test/2", Title: "t2-B", Text: "Самостоятельная новость", Source: "D"}
	dd := &fakeDeduper{clusters: []llm.DupCluster{{Indices: []int{1, 2}, Primary: 1}}}
	p := newTestPipeline(st, &fakeClassifier{}, &fakePoster{}, []TrackedSource{
		{Src: fakeSource{name: "a", items: []feed.Item{t1a, t1b}}, SourceID: 1, TopicID: 1},
		{Src: fakeSource{name: "b", items: []feed.Item{t2a, t2b}}, SourceID: 2, TopicID: fiveID},
	}, testNow)
	p.Dedup = dd

	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := dd.callCount(); n != 1 {
		t.Fatalf("B calls = %d, want 1 (в теме Five uncls из 1 элемента — B не вызывается)", n)
	}
	rows, err := st.ListRunDigests(ctx, testNow.Unix())
	if err != nil {
		t.Fatalf("ListRunDigests: %v", err)
	}
	if len(rows) != 2 || rows[0].TopicID != 1 || rows[1].TopicID != fiveID {
		t.Fatalf("rows = %+v, want topics [1 Five]", rows)
	}
	ditems1, err := st.ListDigestItems(ctx, rows[0].ID)
	if err != nil {
		t.Fatalf("ListDigestItems: %v", err)
	}
	if len(ditems1) != 1 || ditems1[0].Title != "t1-A" {
		t.Errorf("topic 1 digest_items = %+v, want only t1-A (t1-B → dropped_dup)", ditems1)
	}
	ditems2, err := st.ListDigestItems(ctx, rows[1].ID)
	if err != nil {
		t.Fatalf("ListDigestItems: %v", err)
	}
	if len(ditems2) != 1 || ditems2[0].Title != "t2-B" {
		t.Errorf("Five digest_items = %+v, want only t2-B (t2-A отброшен из кеша dropped_dup)", ditems2)
	}
	states, err := st.GetMessageStates(ctx, []string{t1b.DedupKey()})
	if err != nil {
		t.Fatalf("GetMessageStates: %v", err)
	}
	if s := states[t1b.DedupKey()]; s.Status != stateDroppedDup || s.ClusterID != testClusterID(1) {
		t.Errorf("dup state = %+v, want dropped_dup/%s", s, testClusterID(1))
	}
	fresh, err := st.FilterUnseen(ctx, []feed.Item{t1a, t1b, t2a, t2b})
	if err != nil {
		t.Fatalf("FilterUnseen: %v", err)
	}
	if len(fresh) != 0 {
		t.Errorf("fresh = %d, want 0 (все seen: delivered + dropped)", len(fresh))
	}
}
