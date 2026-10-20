package digest

import (
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/akomyagin/aiTelegaBot/internal/storage"
)

var updateGoldens = flag.Bool("update", false, "rewrite golden files under testdata/")

func compareGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *updateGoldens {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run `go test ./internal/digest -update` once): %v", path, err)
	}
	if got != string(want) {
		t.Errorf("%s mismatch:\n got: %q\nwant: %q", name, got, string(want))
	}
}

var goldenNow = time.Date(2024, 1, 15, 9, 0, 0, 0, time.UTC)

func TestRenderTopicBody_Golden(t *testing.T) {
	tests := []struct {
		name     string
		topic    string
		headline string
		count    int
		titles   []string
	}{
		{"topic_body_normal", "AI & ML <news>", "Главное: релиз X; обзор Y.", 3, nil},
		{"topic_body_notopic", "Без темы", "Сводка без темы.", 2, nil},
		// 12 titles — only the first 10 are rendered (fallback cap).
		{"topic_body_empty_headline", "Go", "", 12,
			[]string{"A <b>", "B", "C", "D", "E", "F", "G", "H", "I", "J", "K", "L"}},
		{"topic_body_empty_headline_no_titles", "Go", "", 2, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RenderTopicBody(tt.topic, tt.headline, tt.count, goldenNow, tt.titles...)
			compareGolden(t, tt.name, got)
		})
	}
}

func TestRenderTopicL1_Golden(t *testing.T) {
	body := RenderTopicBody("Тема <1>", "Заголовок & суть.", 2, goldenNow)
	items := []storage.DigestItem{
		{Title: "Первый материал <b>&</b> " + strings.Repeat("x", 150)},
		{Title: "Второй"},
	}

	t.Run("normal", func(t *testing.T) {
		compareGolden(t, "topic_l1_normal", RenderTopicL1(body, items))
	})
	t.Run("empty_items", func(t *testing.T) {
		got := RenderTopicL1(body, nil)
		compareGolden(t, "topic_l1_empty_items", got)
		if got != body {
			t.Errorf("empty items must return the body AS IS")
		}
	})
}

func TestRenderTopicL1_Overflow(t *testing.T) {
	t.Run("items_cut", func(t *testing.T) {
		body := strings.Repeat("а", 3900)
		items := make([]storage.DigestItem, 30)
		for i := range items {
			items[i] = storage.DigestItem{Title: "заголовок материала номер " + string(rune('A'+i%26))}
		}
		got := RenderTopicL1(body, items)
		if n := utf8.RuneCountInString(got); n > 4096 {
			t.Errorf("rune count = %d, want ≤4096", n)
		}
		if !strings.Contains(got, "…и ещё ") {
			t.Errorf("missing «…и ещё K» marker, tail: %q", got[max(0, len(got)-120):])
		}
		if !strings.Contains(got, "• заголовок материала номер A") {
			t.Error("the first item line must survive the tail cut")
		}
	})
	t.Run("body_cut", func(t *testing.T) {
		body := strings.Repeat("б", 5000)
		got := RenderTopicL1(body, []storage.DigestItem{{Title: "T"}})
		if n := utf8.RuneCountInString(got); n != 4096 {
			t.Errorf("rune count = %d, want exactly 4096", n)
		}
		if !strings.HasSuffix(got, "…") {
			t.Error("body cut must end with …")
		}
	})
	// Review C1 M1 regression: bodies in the ~4081–4094-rune band made the
	// old code panic (slice r[:4095] on a shorter body) when even the
	// marker-only tail did not fit. The body alone fits → returned as is.
	t.Run("body_in_panic_band_returned_as_is", func(t *testing.T) {
		for _, n := range []int{4085, 4090, 4094, 4095, 4096} {
			body := strings.Repeat("в", n)
			got := RenderTopicL1(body, []storage.DigestItem{{Title: "Длинный тайтл"}})
			if r := utf8.RuneCountInString(got); r > 4096 {
				t.Errorf("n=%d: rune count = %d, want ≤4096", n, r)
			}
			if !strings.HasPrefix(got, body) {
				t.Errorf("n=%d: body must survive verbatim when it fits", n)
			}
		}
	})
}

// Review C1 M2: RenderTopicBody caps the stored body — headline to 800 runes,
// fallback titles to 120 each — so digests.body cannot approach the 4096
// message limit (the Summarize-fallback and offline headlines are uncapped
// upstream).
func TestRenderTopicBody_Caps(t *testing.T) {
	now := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	t.Run("long_headline_capped", func(t *testing.T) {
		headline := strings.Repeat("x", 900)
		got := RenderTopicBody("Тема", headline, 3, now)
		if strings.Contains(got, strings.Repeat("x", 801)) {
			t.Error("headline must be capped at 800 runes")
		}
		if !strings.Contains(got, strings.Repeat("x", 800)) {
			t.Error("the first 800 headline runes must survive the cap")
		}
		if n := utf8.RuneCountInString(got); n > 4096 {
			t.Errorf("body rune count = %d, want ≤4096", n)
		}
	})
	t.Run("long_fallback_titles_capped", func(t *testing.T) {
		titles := make([]string, 10)
		for i := range titles {
			titles[i] = strings.Repeat("ы", 200)
		}
		got := RenderTopicBody("Тема", "", 10, now, titles...)
		if strings.Contains(got, strings.Repeat("ы", 121)) {
			t.Error("fallback titles must be capped at 120 runes each")
		}
		if n := utf8.RuneCountInString(got); n > 4096 {
			t.Errorf("body rune count = %d, want ≤4096", n)
		}
	})
}

func TestRenderTOC_Golden(t *testing.T) {
	one := []TOCEntry{{DigestID: 11, TopicName: "AI & <ML>", ItemCount: 4}}
	ten := make([]TOCEntry, 10)
	for i := range ten {
		ten[i] = TOCEntry{DigestID: int64(100 + i), TopicName: "Тема " + strconv.Itoa(i+1), ItemCount: i + 1}
	}
	compareGolden(t, "toc_one", RenderTOC(one, "15 Jan 2024"))
	compareGolden(t, "toc_ten", RenderTOC(ten, "15 Jan 2024"))
	compareGolden(t, "toc_nodate", RenderTOC(one, ""))
}

func TestTOCButtons(t *testing.T) {
	entries := []TOCEntry{
		{DigestID: 7, TopicName: strings.Repeat("тема", 10)}, // 40 cyrillic runes = 80 bytes > budget
		{DigestID: 8, TopicName: strings.Repeat("😀", 15)},    // 60 bytes > budget, 4-byte runes
		{DigestID: 9, TopicName: "Короткая"},                 // fits
	}
	rows := TOCButtons(entries)
	if len(rows) != len(entries) {
		t.Fatalf("rows = %d, want %d (one button per row)", len(rows), len(entries))
	}
	for i, row := range rows {
		if len(row) != 1 {
			t.Fatalf("row %d holds %d buttons, want 1", i, len(row))
		}
		btn := row[0]
		if want := "T:" + strconv.FormatInt(entries[i].DigestID, 10); btn.Data != want {
			t.Errorf("row %d data = %q, want %q", i, btn.Data, want)
		}
		if !strings.HasPrefix(btn.Text, topicOpenPrefix) {
			t.Errorf("row %d text = %q, want prefix «Открыть: »", i, btn.Text)
		}
		if len(btn.Text) > 64 {
			t.Errorf("row %d text = %d BYTES, want ≤64 (урок A2 H1)", i, len(btn.Text))
		}
		if !utf8.ValidString(btn.Text) {
			t.Errorf("row %d text is not valid UTF-8 — rune-boundary cut broken", i)
		}
	}
	if got := rows[2][0].Text; got != "Открыть: Короткая" {
		t.Errorf("short name must be untouched, got %q", got)
	}
	if !strings.HasSuffix(rows[0][0].Text, "…") || !strings.HasSuffix(rows[1][0].Text, "…") {
		t.Error("over-budget labels must end with …")
	}
}
