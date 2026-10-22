// Этап 9D-2a: TOC-восстановление читает runs.summary (GetRunSummary) →
// overall-блок RenderTOC; нет строки runs → блок отсутствует (legacy-прогоны).

package app

import (
	"context"
	"strings"
	"testing"

	"github.com/akomyagin/aiTelegaBot/internal/telegram"
)

func TestNewDigestViewer_TOCOverall(t *testing.T) {
	st := newForwardTestStore(t)
	ctx := context.Background()
	viewer := newDigestViewer(st)

	if _, err := st.InsertDigest(ctx, 889, 1, "B", 1, ""); err != nil {
		t.Fatalf("InsertDigest: %v", err)
	}
	if err := st.UpsertRunSummary(ctx, 889, "ОБЩЕЕ САММАРИ ВЫПУСКА"); err != nil {
		t.Fatalf("UpsertRunSummary: %v", err)
	}
	view, ok, err := viewer(ctx, telegram.DigestViewRequest{Kind: telegram.ViewKindTOC, ID: 889})
	if err != nil || !ok {
		t.Fatalf("viewer(toc) = ok:%v err:%v", ok, err)
	}
	if !strings.Contains(view.Text, "ОБЩЕЕ САММАРИ ВЫПУСКА") {
		t.Errorf("TOC lacks overall block:\n%s", view.Text)
	}

	// Нет строки runs → overall-блок отсутствует (legacy).
	if _, err := st.InsertDigest(ctx, 890, 1, "B2", 1, ""); err != nil {
		t.Fatalf("InsertDigest: %v", err)
	}
	view, ok, err = viewer(ctx, telegram.DigestViewRequest{Kind: telegram.ViewKindTOC, ID: 890})
	if err != nil || !ok {
		t.Fatalf("viewer(toc legacy) = ok:%v err:%v", ok, err)
	}
	if strings.Contains(view.Text, "ОБЩЕЕ") {
		t.Errorf("legacy TOC must not carry overall:\n%s", view.Text)
	}
}
