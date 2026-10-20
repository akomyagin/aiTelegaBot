// Digest navigation viewer (Stage 8 C, план §C): замыкание поверх storage +
// рендеров digest для telegram-опции WithDigestViewer. Все данные строго из
// SQLite — после рестарта бота клики T:/TOC: работают без in-memory снапшотов
// прогона (rest-семантика §4.3). Ошибки storage → (…, false, err); crafted-id
// не роняют viewer (GetDigest ok=false — не ошибка).

package app

import (
	"context"
	"strconv"
	"time"

	"github.com/akomyagin/aiTelegaBot/internal/digest"
	"github.com/akomyagin/aiTelegaBot/internal/storage"
	"github.com/akomyagin/aiTelegaBot/internal/telegram"
)

// newDigestViewer builds the DigestViewer over the store:
//   - ViewKindTopic (ID = digestID): GetDigest + ListDigestItems →
//     RenderTopicL1; кнопка «◀ К оглавлению» (TOC:<runID>) только при
//     RunID != 0 — у легаси-строк (run_id=0) оглавления нет.
//   - ViewKindTOC (ID = runID): ListRunDigests + ListTopics → RenderTOC
//     (date из CreatedAt первой строки; ошибка парсинга → date="") +
//     TOCButtons. runID=0 → пусто → not found.
func newDigestViewer(store *storage.SQLiteStore) telegram.DigestViewer {
	return func(ctx context.Context, req telegram.DigestViewRequest) (telegram.DigestView, bool, error) {
		switch req.Kind {
		case telegram.ViewKindTopic:
			d, ok, err := store.GetDigest(ctx, req.ID)
			if err != nil {
				return telegram.DigestView{}, false, err
			}
			if !ok {
				return telegram.DigestView{}, false, nil
			}
			items, err := store.ListDigestItems(ctx, req.ID)
			if err != nil {
				return telegram.DigestView{}, false, err
			}
			view := telegram.DigestView{Text: digest.RenderTopicL1(d.Body, items)}
			if d.RunID != 0 {
				view.Buttons = [][]telegram.InlineButton{{
					{Text: "◀ К оглавлению", Data: "TOC:" + strconv.FormatInt(d.RunID, 10)},
				}}
			}
			return view, true, nil

		case telegram.ViewKindTOC:
			if req.ID == 0 {
				// run_id=0 = «не прогон» (легаси-строки): оглавления нет.
				return telegram.DigestView{}, false, nil
			}
			rows, err := store.ListRunDigests(ctx, req.ID)
			if err != nil {
				return telegram.DigestView{}, false, err
			}
			if len(rows) == 0 {
				return telegram.DigestView{}, false, nil
			}
			topics, err := store.ListTopics(ctx)
			if err != nil {
				return telegram.DigestView{}, false, err
			}
			names := make(map[int64]string, len(topics))
			for _, t := range topics {
				names[t.ID] = t.Name
			}
			entries := make([]digest.TOCEntry, 0, len(rows))
			for _, d := range rows {
				name, ok := names[d.TopicID]
				if !ok {
					// FK не enforcement'ится — висячий topic_id не роняет view.
					name = "Тема #" + strconv.FormatInt(d.TopicID, 10)
				}
				entries = append(entries, digest.TOCEntry{
					DigestID: d.ID, TopicName: name, ItemCount: d.ItemCount,
				})
			}
			date := ""
			if created, err := time.Parse(time.RFC3339, rows[0].CreatedAt); err == nil {
				date = created.Format("02 Jan 2006")
			}
			return telegram.DigestView{
				Text:    digest.RenderTOC(entries, date),
				Buttons: digest.TOCButtons(entries),
			}, true, nil
		}
		// Неизвестный kind — «не найдено» (не ошибка).
		return telegram.DigestView{}, false, nil
	}
}
