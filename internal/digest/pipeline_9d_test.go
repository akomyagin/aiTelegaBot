// Pipeline-тесты Этапа 9D-2b (план, тест-кейс 8 подэтапа 9D): шаги 4d/6/7 —
// вызов C (headline темы + sections_json), вызов D (runs.summary + overall в
// TOC), деградации (сбой C → fallback-headline, тема НЕ пропускается; сбой D →
// TOC без overall, Run=nil, runs-строки нет). Реальный Run поверх временной
// SQLite + фейки A/B/C/D/E/F со счётчиками. Обратная совместимость
// Aggregate=nil/Overall=nil покрыта ВСЕМ существующим pipeline_test.go (9A/9B)
// и dedup_pipeline_test.go (9C): нил-сейвы пропускают шаги 4d/6 целиком.

package digest

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
	"github.com/akomyagin/aiTelegaBot/internal/llm"
)

// --- фейки вызовов C/D (9D) ---

// fakeTopicAggregator — фейк вызова C со счётчиком и записью входов. Дефолт:
// headline = "headline: <тема>", по секции "sec-<sourceID>" на блок входа;
// byTopic переопределяет результат; failTopics/err рвут вызов (транспорт).
type fakeTopicAggregator struct {
	mu         sync.Mutex
	calls      int
	topics     []string
	sources    [][]llm.SourceBlock
	byTopic    map[string]llm.TopicAggregate
	failTopics map[string]bool
	err        error
}

func (f *fakeTopicAggregator) AggregateTopic(ctx context.Context,
	topicName string, sources []llm.SourceBlock) (llm.TopicAggregate, error) {
	f.mu.Lock()
	f.calls++
	f.topics = append(f.topics, topicName)
	cp := make([]llm.SourceBlock, len(sources))
	copy(cp, sources)
	f.sources = append(f.sources, cp)
	fail := f.err != nil || f.failTopics[topicName]
	f.mu.Unlock()
	if fail {
		return llm.TopicAggregate{}, errors.New("C boom: " + topicName)
	}
	if agg, ok := f.byTopic[topicName]; ok {
		return agg, nil
	}
	agg := llm.TopicAggregate{Headline: "headline: " + topicName}
	for _, s := range sources {
		agg.Sections = append(agg.Sections, llm.TopicHeadline{
			SourceID: s.SourceID,
			Summary:  "sec-" + strconv.FormatInt(s.SourceID, 10),
		})
	}
	return agg, nil
}

func (f *fakeTopicAggregator) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeOverallAggregator — фейк вызова D со счётчиком и записью входов.
// Дефолт summary = "Общее саммари выпуска."; err рвёт вызов (транспорт);
// empty → ("", nil) — деградация двойного мусора (контракт OverallAggregator).
type fakeOverallAggregator struct {
	mu      sync.Mutex
	calls   int
	topics  [][]llm.OverallTopic
	summary string
	err     error
	empty   bool
}

func (f *fakeOverallAggregator) AggregateOverall(ctx context.Context,
	topics []llm.OverallTopic) (string, error) {
	f.mu.Lock()
	f.calls++
	cp := make([]llm.OverallTopic, len(topics))
	copy(cp, topics)
	f.topics = append(f.topics, cp)
	err, empty := f.err, f.empty
	f.mu.Unlock()
	if err != nil {
		return "", err
	}
	if empty {
		return "", nil
	}
	if f.summary != "" {
		return f.summary, nil
	}
	return "Общее саммари выпуска.", nil
}

func (f *fakeOverallAggregator) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// prefixDeduper — фейк вызова B с кластерами по префиксу заголовка ПЕРВОГО
// элемента батча (fakeDeduper из dedup_pipeline_test.go возвращает одни
// кластеры всем темам — здесь темы разной длины).
type prefixDeduper struct {
	mu       sync.Mutex
	calls    int
	byPrefix map[string][]llm.DupCluster
}

func (f *prefixDeduper) FindDuplicates(ctx context.Context,
	items []llm.DedupItem) ([]llm.DupCluster, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if len(items) == 0 {
		return nil, nil
	}
	return f.byPrefix[titlePrefix(items[0].Title)], nil
}

func (f *prefixDeduper) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// navRunItem (не navItem — тот занят в render_nav_test.go) — элемент tg-источника со ссылкой-статьёй и явным Source (имя
// топика для SourceBlock.Name — linkItem его не заполняет).
func navRunItem(ch, id, title, art, source string) feed.Item {
	it := linkItem(ch, id, title, art)
	it.Source = source
	return it
}

// --- кейс 8 плана (строки 1010–1013): полный Run 9A–9D ---
//
// 2 темы × 2 топика (источника); ad и dup отброшены; счётчики A,B,C,E,F —
// ровно по одному на тему, D — один на прогон; delivered TOC содержит overall;
// runs.summary записан; sections_json round-trips через ParseTopicSections;
// порядок SourceBlock = порядок collect (чередующиеся источники).

func TestPipelineRun_FullRun9D(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	fiveID, err := st.CreateTopic(ctx, "Five")
	if err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	bezTemy := topicName(t, st, 1)

	// Тема 1: a1(src11) a2(src12) a3(src11, дубль a1) a4(src12) + ad.
	a1 := navRunItem("c1", "1", "t1-T1", "https://art.test/a1", "ch11")
	a2 := navRunItem("c1", "2", "t1-T2", "https://art.test/a2", "ch12")
	a3 := navRunItem("c1", "3", "t1-T3", "https://art.test/a3", "ch11")
	a4 := navRunItem("c1", "4", "t1-T4", "https://art.test/a4", "ch12")
	ad := navRunItem("c1", "5", "ad-T1", "https://art.test/ad", "ch13")
	// Тема Five: b1(src21) b2(src22).
	b1 := navRunItem("c2", "1", "t2-T1", "https://art.test/b1", "ch21")
	b2 := navRunItem("c2", "2", "t2-T2", "https://art.test/b2", "ch22")
	all := []feed.Item{a1, a2, a3, a4, ad, b1, b2}

	srcs := []TrackedSource{
		{Src: fakeSource{name: "ch11", items: []feed.Item{a1}}, SourceID: 11, TopicID: 1},
		{Src: fakeSource{name: "ch12", items: []feed.Item{a2}}, SourceID: 12, TopicID: 1},
		{Src: fakeSource{name: "ch11", items: []feed.Item{a3}}, SourceID: 11, TopicID: 1},
		{Src: fakeSource{name: "ch12", items: []feed.Item{a4}}, SourceID: 12, TopicID: 1},
		{Src: fakeSource{name: "ch13", items: []feed.Item{ad}}, SourceID: 13, TopicID: 1},
		{Src: fakeSource{name: "ch21", items: []feed.Item{b1}}, SourceID: 21, TopicID: fiveID},
		{Src: fakeSource{name: "ch22", items: []feed.Item{b2}}, SourceID: 22, TopicID: fiveID},
	}

	cls := &fakeClassifier{byTitle: map[string]fakeOutcome{
		"ad-T1": {class: llm.ClassAd},
	}}
	dd := &prefixDeduper{byPrefix: map[string][]llm.DupCluster{
		// dItems темы 1 (без ad): 1=a1, 2=a2, 3=a3, 4=a4 → a3 дубль a1.
		"t1": {{Indices: []int{1, 3}, Primary: 1}},
	}}
	e := &fakeLinkClassifier{}
	f := &fakeLinkSummarizer{}
	lf := &fakeLinkFetcher{}
	c := &fakeTopicAggregator{}
	d := &fakeOverallAggregator{}
	poster := &fakePoster{}

	p := linkTestPipeline(st, cls, poster, srcs, e, f, lf, 5, 30)
	p.Dedup, p.Aggregate, p.Overall = dd, c, d
	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Счётчики: A,B,C,E,F — по одному на тему (2), D — один на прогон.
	if cls.callCount() != 2 || dd.callCount() != 2 || c.callCount() != 2 ||
		e.callCount() != 2 || f.callCount() != 2 {
		t.Fatalf("counters A/B/C/E/F = %d/%d/%d/%d/%d, want 2/2/2/2/2",
			cls.callCount(), dd.callCount(), c.callCount(), e.callCount(), f.callCount())
	}
	if d.callCount() != 1 {
		t.Fatalf("D calls = %d, want exactly 1 per run", d.callCount())
	}

	// Вход C темы 1: блоки в порядке COLLECT — [11, 12]; саммари src12 —
	// чередующиеся элементы (a2,a4) в порядке collect; саммари ПОСЛЕ шага 4c
	// (с ↳-добавкой — 4d идёт после 4c, базовое саммари — префикс).
	if len(c.sources) != 2 {
		t.Fatalf("C inputs = %d, want 2", len(c.sources))
	}
	blocks := c.sources[0]
	if len(blocks) != 2 || blocks[0].SourceID != 11 || blocks[1].SourceID != 12 {
		t.Fatalf("topic1 blocks = %+v, want source_ids [11 12] in collect order", blocks)
	}
	if blocks[0].Name != "ch11" || blocks[1].Name != "ch12" {
		t.Errorf("block names = (%q,%q), want (ch11,ch12)", blocks[0].Name, blocks[1].Name)
	}
	if len(blocks[0].Summaries) != 1 || !strings.HasPrefix(blocks[0].Summaries[0], "s1") {
		t.Errorf("src11 summaries = %q, want one starting with s1 (a1; a3 — дубль)", blocks[0].Summaries)
	}
	if len(blocks[1].Summaries) != 2 || !strings.HasPrefix(blocks[1].Summaries[0], "s2") ||
		!strings.HasPrefix(blocks[1].Summaries[1], "s4") {
		t.Errorf("src12 summaries = %q, want [s2…, s4…] in collect order", blocks[1].Summaries)
	}

	// Вход D: темы в порядке entries, headline'ы из C, ItemCount — kept.
	if len(d.topics) != 1 || len(d.topics[0]) != 2 {
		t.Fatalf("D inputs = %+v, want one call with 2 topics", d.topics)
	}
	if got := d.topics[0][0]; got.Name != bezTemy || got.ItemCount != 3 ||
		got.Headline != "headline: "+bezTemy {
		t.Errorf("D topic[0] = %+v, want {%s 3 headline: %s}", got, bezTemy, bezTemy)
	}
	if got := d.topics[0][1]; got.Name != "Five" || got.ItemCount != 2 ||
		got.Headline != "headline: Five" {
		t.Errorf("D topic[1] = %+v, want {Five 2 headline: Five}", got)
	}

	// Delivered TOC содержит overall-блок.
	if poster.calls() != 1 {
		t.Fatalf("poster calls = %d, want 1", poster.calls())
	}
	toc := poster.texts[0]
	if !strings.Contains(toc, "Тем: 2") || !strings.Contains(toc, "Общее саммари выпуска.") {
		t.Errorf("TOC lacks «Тем: 2» или overall-блок:\n%s", toc)
	}

	// runs.summary записан.
	runID := testNow.Unix()
	sum, ok, err := st.GetRunSummary(ctx, runID)
	if err != nil || !ok || sum != "Общее саммари выпуска." {
		t.Errorf("GetRunSummary = (%q,%v,%v), want («Общее саммари выпуска.»,true,nil)", sum, ok, err)
	}

	// Строки digests: headline в body, sections_json round-trips.
	rows, err := st.ListRunDigests(ctx, runID)
	if err != nil {
		t.Fatalf("ListRunDigests: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2", rows)
	}
	for i, want := range []struct {
		topicID  int64
		count    int
		headline string
		sections map[int64]string
	}{
		{1, 3, "headline: " + bezTemy, map[int64]string{11: "sec-11", 12: "sec-12"}},
		{fiveID, 2, "headline: Five", map[int64]string{21: "sec-21", 22: "sec-22"}},
	} {
		r := rows[i]
		if r.TopicID != want.topicID || r.ItemCount != want.count || !r.Delivered {
			t.Fatalf("row[%d] = %+v, want topic %d count %d delivered", i, r, want.topicID, want.count)
		}
		if !strings.Contains(r.Body, want.headline) {
			t.Errorf("row[%d] body lacks %q:\n%s", i, want.headline, r.Body)
		}
		got := ParseTopicSections(r.SectionsJSON)
		if len(got) != len(want.sections) {
			t.Fatalf("row[%d] sections = %v (json %q), want %v", i, got, r.SectionsJSON, want.sections)
		}
		for sid, s := range want.sections {
			if got[sid] != s {
				t.Errorf("row[%d] sections[%d] = %q, want %q", i, sid, got[sid], s)
			}
		}
	}

	// ad и dup отброшены: digest_items темы 1 — a1,a2,a4 (без a3/ad).
	ditems, err := st.ListDigestItems(ctx, rows[0].ID)
	if err != nil {
		t.Fatalf("ListDigestItems: %v", err)
	}
	if len(ditems) != 3 || ditems[0].Title != "t1-T1" || ditems[1].Title != "t1-T2" ||
		ditems[2].Title != "t1-T4" {
		t.Errorf("digest_items = %+v, want t1-T1,t1-T2,t1-T4 (dup и ad отброшены)", ditems)
	}
	states, err := st.GetMessageStates(ctx, dedupKeys(all))
	if err != nil {
		t.Fatalf("GetMessageStates: %v", err)
	}
	if s := states[ad.DedupKey()]; s.Status != "dropped_ad" {
		t.Errorf("ad state = %+v, want dropped_ad", s)
	}
	if s := states[a3.DedupKey()]; s.Status != "dropped_dup" || s.IsPrimary {
		t.Errorf("dup state = %+v, want dropped_dup non-primary", s)
	}
	fresh, err := st.FilterUnseen(ctx, all)
	if err != nil {
		t.Fatalf("FilterUnseen: %v", err)
	}
	if len(fresh) != 0 {
		t.Errorf("fresh = %+v, want empty (все seen, включая ad/dup)", fresh)
	}
}

// --- сбой C: тема доставлена с fallback-headline, НЕ пропущена (кейс 8) ---

func TestPipelineRun_AggregateErrorFallbackHeadline(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	items := []feed.Item{
		navRunItem("f", "1", "f-T1", "https://art.test/f1", "chF"),
		navRunItem("f", "2", "f-T2", "https://art.test/f2", "chF"),
	}
	c := &fakeTopicAggregator{err: errors.New("transport boom")}
	d := &fakeOverallAggregator{}
	poster := &fakePoster{}
	p := newTestPipeline(st, &fakeClassifier{}, poster, []TrackedSource{
		{Src: fakeSource{name: "f", items: items}, SourceID: 31, TopicID: 1},
	}, testNow)
	p.Aggregate, p.Overall = c, d
	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run = %v, want nil (сбой C — деградация, не пропуск темы)", err)
	}
	if c.callCount() != 1 || poster.calls() != 1 {
		t.Fatalf("C calls = %d, poster = %d, want 1/1 (тема доставлена)", c.callCount(), poster.calls())
	}
	rows, err := st.ListRunDigests(ctx, testNow.Unix())
	if err != nil {
		t.Fatalf("ListRunDigests: %v", err)
	}
	if len(rows) != 1 || rows[0].ItemCount != 2 {
		t.Fatalf("rows = %+v, want one row with both items", rows)
	}
	// fallback-headline: первые ≤3 саммари списком «- …» (§4.7).
	if !strings.Contains(rows[0].Body, "- s1") || !strings.Contains(rows[0].Body, "- s2") {
		t.Errorf("body lacks fallback «- s1»/«- s2»:\n%s", rows[0].Body)
	}
	// sections пусты → sections_json пуст (L1 деградирует на строки без саммари).
	if rows[0].SectionsJSON != "" {
		t.Errorf("sections_json = %q, want empty on C failure", rows[0].SectionsJSON)
	}
	if got := ParseTopicSections(rows[0].SectionsJSON); len(got) != 0 {
		t.Errorf("ParseTopicSections = %v, want empty", got)
	}
	// D получает fallback-headline темы; прогон не прерван.
	if d.callCount() != 1 {
		t.Fatalf("D calls = %d, want 1 (сбой C не отменяет шаг 6)", d.callCount())
	}
	if h := d.topics[0][0].Headline; h != "- s1\n- s2" {
		t.Errorf("D headline = %q, want fallback «- s1\\n- s2»", h)
	}
}

// --- сбой D: TOC без overall, Run=nil, runs-строки НЕТ (решение 9D-2b:
// UpsertRunSummary только при непустом summary) ---

func TestPipelineRun_OverallFailureDegrades(t *testing.T) {
	cases := []struct {
		name string
		fail func(d *fakeOverallAggregator)
	}{
		{"transport error", func(d *fakeOverallAggregator) { d.err = errors.New("D boom") }},
		{"empty summary", func(d *fakeOverallAggregator) { d.empty = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := openTestStore(t)
			ctx := context.Background()
			items := []feed.Item{navRunItem("g", "1", "g-T1", "https://art.test/g1", "chG")}
			d := &fakeOverallAggregator{}
			tc.fail(d)
			poster := &fakePoster{}
			p := newTestPipeline(st, &fakeClassifier{}, poster, []TrackedSource{
				{Src: fakeSource{name: "g", items: items}, SourceID: 41, TopicID: 1},
			}, testNow)
			p.Aggregate, p.Overall = &fakeTopicAggregator{}, d
			if err := p.Run(ctx); err != nil {
				t.Fatalf("Run = %v, want nil (сбой D — log-only деградация)", err)
			}
			if d.callCount() != 1 || poster.calls() != 1 {
				t.Fatalf("D calls = %d, poster = %d, want 1/1", d.callCount(), poster.calls())
			}
			if strings.Contains(poster.texts[0], "Общее саммари") {
				t.Errorf("TOC must not contain the overall block:\n%s", poster.texts[0])
			}
			sum, ok, err := st.GetRunSummary(ctx, testNow.Unix())
			if err != nil {
				t.Fatalf("GetRunSummary: %v", err)
			}
			if ok || sum != "" {
				t.Errorf("GetRunSummary = (%q,%v), want miss (строки runs нет)", sum, ok)
			}
		})
	}
}

// --- юниты хелперов шага 4d ---

// TestSourceBlocksFromKept_CollectOrder: чередующиеся источники → блоки в
// порядке ПЕРВОГО появления, саммари внутри блока — в порядке collect.
func TestSourceBlocksFromKept_CollectOrder(t *testing.T) {
	mk := func(sid int64, src, summary string) keptItem {
		it := feed.Item{SourceID: sid, Source: src}
		return keptItem{it: it, summary: summary}
	}
	kept := []keptItem{
		mk(7, "A", "s1"), mk(8, "B", "s2"), mk(7, "A", "s3"), mk(8, "B", "s4"), mk(7, "A", "s5"),
	}
	blocks := sourceBlocksFromKept(kept)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %+v, want 2", blocks)
	}
	if blocks[0].SourceID != 7 || blocks[1].SourceID != 8 {
		t.Fatalf("block order = (%d,%d), want (7,8) — первое появление", blocks[0].SourceID, blocks[1].SourceID)
	}
	if blocks[0].Name != "A" || blocks[1].Name != "B" {
		t.Errorf("names = (%q,%q), want (A,B)", blocks[0].Name, blocks[1].Name)
	}
	want := [][]string{{"s1", "s3", "s5"}, {"s2", "s4"}}
	for i, b := range blocks {
		if len(b.Summaries) != len(want[i]) {
			t.Fatalf("block[%d] summaries = %q, want %q", i, b.Summaries, want[i])
		}
		for j := range b.Summaries {
			if b.Summaries[j] != want[i][j] {
				t.Errorf("block[%d][%d] = %q, want %q", i, j, b.Summaries[j], want[i][j])
			}
		}
	}
	if got := sourceBlocksFromKept(nil); len(got) != 0 {
		t.Errorf("nil kept → %+v, want empty", got)
	}
}

// TestFallbackHeadline: первые ≤3 непустых саммари списком «- …».
func TestFallbackHeadline(t *testing.T) {
	mk := func(s string) keptItem { return keptItem{summary: s} }
	got := fallbackHeadline([]keptItem{mk("a"), mk("  "), mk("b"), mk("c"), mk("d")})
	if want := "- a\n- b\n- c"; got != want {
		t.Errorf("fallbackHeadline = %q, want %q (≤3 непустых)", got, want)
	}
	if got := fallbackHeadline(nil); got != "" {
		t.Errorf("empty kept → %q, want «»", got)
	}
}

// TestTopicSectionsJSON: порядок = порядок blocks (collect), дубль — первый
// выигрыш, пустой summary → секции нет, пустой результат → "" (не «[]»);
// round-trip через ParseTopicSections (контракт 9D-2a).
func TestTopicSectionsJSON(t *testing.T) {
	blocks := []llm.SourceBlock{{SourceID: 3}, {SourceID: 1}, {SourceID: 2}}
	sections := []llm.TopicHeadline{
		{SourceID: 2, Summary: "два"},
		{SourceID: 1, Summary: "один"},
		{SourceID: 1, Summary: "дубль"}, // отброшен (первый выигрыш)
		{SourceID: 3, Summary: "  "},    // пусто → секции нет
		{SourceID: 9, Summary: "чужой"}, // вне blocks → не сериализуется
	}
	got := topicSectionsJSON(blocks, sections)
	want := `[{"source_id":1,"summary":"один"},{"source_id":2,"summary":"два"}]`
	if got != want {
		t.Errorf("topicSectionsJSON = %s, want %s (порядок collect)", got, want)
	}
	rt := ParseTopicSections(got)
	if len(rt) != 2 || rt[1] != "один" || rt[2] != "два" {
		t.Errorf("round-trip = %v, want {1:один, 2:два}", rt)
	}
	if s := topicSectionsJSON(blocks, nil); s != "" {
		t.Errorf("no sections → %q, want «»", s)
	}
	if s := topicSectionsJSON(blocks, []llm.TopicHeadline{{SourceID: 3, Summary: " x "}}); s !=
		`[{"source_id":3,"summary":"x"}]` {
		t.Errorf("trim = %q, want trimmed summary", s)
	}
}

// TestInsertDigestSectionsRoundTrip: sections_json, записанный pipeline-форматом,
// читается GetDigest (миграция v6) и round-trips через ParseTopicSections.
func TestInsertDigestSectionsRoundTrip(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	sj := topicSectionsJSON(
		[]llm.SourceBlock{{SourceID: 5}, {SourceID: 6}},
		[]llm.TopicHeadline{{SourceID: 5, Summary: "пять"}, {SourceID: 6, Summary: "шесть"}},
	)
	id, err := st.InsertDigest(ctx, 999, 1, "body", 2, sj)
	if err != nil {
		t.Fatalf("InsertDigest: %v", err)
	}
	d, ok, err := st.GetDigest(ctx, id)
	if err != nil || !ok {
		t.Fatalf("GetDigest: ok=%v err=%v", ok, err)
	}
	if d.SectionsJSON != sj {
		t.Fatalf("stored = %q, want %q", d.SectionsJSON, sj)
	}
	got := ParseTopicSections(d.SectionsJSON)
	if got[5] != "пять" || got[6] != "шесть" || len(got) != 2 {
		t.Errorf("ParseTopicSections = %v, want {5:пять, 6:шесть}", got)
	}
}
