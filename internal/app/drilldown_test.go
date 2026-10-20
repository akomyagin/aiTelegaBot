// Дым-тест wiring'а D-2 (app.go): digest.NewDrillDown(store, store.ListSources)
// поверх реальной temp-SQLite — InsertDigest → MarkDelivered → L2:/L3: data →
// CallbackResult с Edit; sources-снапшот даёт имена источников.

package app

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/akomyagin/aiTelegaBot/internal/digest"
	"github.com/akomyagin/aiTelegaBot/internal/storage"
)

func TestDrillDownWiringSmoke(t *testing.T) {
	ctx := context.Background()
	st, err := storage.Open(filepath.Join(t.TempDir(), "drill.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer st.Close()

	topicID, err := st.CreateTopic(ctx, "AI")
	if err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	srcID, err := st.AddSource(ctx, "rss", "https://example.com/feed.xml")
	if err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	if _, err := st.SetSourceTopic(ctx, srcID, topicID); err != nil {
		t.Fatalf("SetSourceTopic: %v", err)
	}
	digestID, err := st.InsertDigest(ctx, 500, topicID, "<b>body</b>", 1)
	if err != nil {
		t.Fatalf("InsertDigest: %v", err)
	}
	err = st.MarkDelivered(ctx, digestID, []storage.DigestItem{{
		SourceID: srcID,
		Title:    "Заголовок <1>",
		URL:      "https://x.example/a?b=1&c=2",
		Summary:  "Сводка",
		Snippet:  "Сниппет",
	}})
	if err != nil {
		t.Fatalf("MarkDelivered: %v", err)
	}

	// Ровно тот seam, который app.go проводит в telegram.WithDrillDown.
	dd := digest.NewDrillDown(st, st.ListSources)

	did := strconv.FormatInt(digestID, 10)
	sid := strconv.FormatInt(srcID, 10)

	l2, err := dd(ctx, "L2:"+did)
	if err != nil {
		t.Fatalf("L2: %v", err)
	}
	if l2 == nil || l2.Edit == nil {
		t.Fatal("L2: want Edit, got nil")
	}
	for _, want := range []string{
		"📰 AI — по источникам",
		"<b>https://example.com/feed.xml</b>", // имя из снапшота ListSources
		"• Сводка",
	} {
		if !strings.Contains(l2.Edit.Text, want) {
			t.Errorf("L2 text %q: missing %q", l2.Edit.Text, want)
		}
	}
	if len(l2.Edit.Buttons) != 2 ||
		l2.Edit.Buttons[0][0].Data != "L3:"+did+":"+sid ||
		l2.Edit.Buttons[1][0].Data != "L1:"+did {
		t.Errorf("L2 buttons: %+v", l2.Edit.Buttons)
	}

	l3, err := dd(ctx, "L3:"+did+":"+sid)
	if err != nil {
		t.Fatalf("L3: %v", err)
	}
	if l3 == nil || l3.Edit == nil {
		t.Fatal("L3: want Edit, got nil")
	}
	for _, want := range []string{
		"📄 https://example.com/feed.xml",
		`<a href="https://x.example/a?b=1&amp;c=2">Заголовок &lt;1&gt;</a> — Сниппет`,
	} {
		if !strings.Contains(l3.Edit.Text, want) {
			t.Errorf("L3 text %q: missing %q", l3.Edit.Text, want)
		}
	}
	if len(l3.Edit.Buttons) != 1 || l3.Edit.Buttons[0][0].Data != "L2:"+did {
		t.Errorf("L3 buttons: %+v", l3.Edit.Buttons)
	}
}
