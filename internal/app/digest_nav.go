// Digest navigation viewer (Stage 8 C / Этап 9D-2a, план §C, §4.6): замыкание
// поверх storage + рендеров digest для telegram-опции WithDigestViewer. Все
// данные строго из SQLite — после рестарта бота клики T:/TOC: работают без
// in-memory снапшотов прогона (rest-семантика §4.3).
//
// T:-view (Этап 9D) — RenderTopicView: кнопки-имена топиков собираются из
// группировки ListDigestItems (порядок = id ASC, первое появление) + снапшота
// sources с peer-Title fallback (digest.SourceDisplayNames) + samмари топиков
// из digests.sections_json (вызов C, миграция v6). TOC-восстановление читает
// runs.summary (GetRunSummary) → overall-блок RenderTOC; нет строки → блок
// отсутствует (legacy-прогоны). Ошибки storage → (…, false, err); crafted-id
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
//   - ViewKindTopic (ID = digestID): GetDigest + ListDigestItems + снапшот
//     sources + sections_json → RenderTopicView; кнопки (имена топиков
//     S:<did>:<sid> + «◀ К оглавлению» TOC:<runID> при RunID != 0) собирает
//     сам рендер — telegram-слой ничего не докомпановывает (9D).
//   - ViewKindTOC (ID = runID): ListRunDigests + ListTopics + GetRunSummary →
//     RenderTOC (date из CreatedAt первой строки; ошибка парсинга → date="";
//     overall из runs.summary, нет строки → пустой overall = блок отсутствует)
//   - TOCButtons. runID=0 → пусто → not found.
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
			names, err := digest.SourceDisplayNames(ctx, store, store.ListSources)
			if err != nil {
				return telegram.DigestView{}, false, err
			}
			nav := digest.RenderTopicView(d.Body, req.ID, d.RunID,
				digest.TopicEntries(items, names, digest.ParseTopicSections(d.SectionsJSON)))
			return telegram.DigestView{Text: nav.Text, Buttons: digest.NavButtons(nav)}, true, nil

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
			// Этап 9D: общее саммари выпуска из runs.summary (вызов D); нет
			// строки → overall пустой → RenderTOC не рендерит блок (legacy).
			overall := ""
			if summary, sumOK, err := store.GetRunSummary(ctx, req.ID); err != nil {
				return telegram.DigestView{}, false, err
			} else if sumOK {
				overall = summary
			}
			return telegram.DigestView{
				Text:    digest.RenderTOC(entries, date, overall),
				Buttons: digest.TOCButtons(entries),
			}, true, nil
		}
		// Неизвестный kind — «не найдено» (не ошибка).
		return telegram.DigestView{}, false, nil
	}
}
