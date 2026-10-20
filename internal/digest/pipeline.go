// Package digest orchestrates one digest run (Stage 8 C, plan §C/§4.11a):
// collect items from all tracked sources → filter unseen → group by topic →
// summarize each topic via llm.TopicSummarizer → persist per-topic digests →
// deliver ONE table-of-contents message with per-topic buttons → mark
// delivered. See docs/plans/stage-8-topics-and-sources.md «Подэтап C».
//
// Delivery gates persistence (§4.3): rows are inserted with delivered=0 and
// flipped by MarkDelivered only after the TOC message reaches the user, so a
// failed delivery is retried (in full) on the next run.
package digest

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
	"github.com/akomyagin/aiTelegaBot/internal/llm"
	"github.com/akomyagin/aiTelegaBot/internal/storage"
	"github.com/akomyagin/aiTelegaBot/internal/telegram"
)

// MessagePoster delivers the TOC message with its inline keyboard. Satisfied
// by *telegram.Bot (DeliverWithButtons).
type MessagePoster interface {
	DeliverWithButtons(ctx context.Context, chatID, text string,
		buttons [][]telegram.InlineButton) error
}

// Pipeline holds the dependencies for one digest run.
type Pipeline struct {
	// SourceProvider returns the tracked source set for THIS run. It is called
	// at the start of every Run, so DB-backed sources added at runtime are
	// picked up without a restart. Must be non-nil.
	SourceProvider func(ctx context.Context) ([]TrackedSource, error)
	Store          storage.Store
	TopicSummary   llm.TopicSummarizer
	Poster         MessagePoster
	ChatID         string
	Log            *slog.Logger     // optional; defaults to slog.Default()
	Now            func() time.Time // optional; defaults to time.Now (for tests)
}

// log returns the configured logger or the default.
func (p *Pipeline) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

// pendingTopic is one summarized+persisted topic awaiting the TOC delivery:
// its digests row id and the digest_items rows MarkDelivered will write.
type pendingTopic struct {
	digestID int64
	items    []storage.DigestItem
}

// Run executes one digest end-to-end — the 9-step delivery model of plan §C.
// Fatal: steps 1–2 (resolve/collect/filter) and 6–7 (all-topics-failed,
// delivery). Per-topic skip: step 5. Log-only: step 8 (§4.9 — a partial
// post-delivery failure never surfaces).
func (p *Pipeline) Run(ctx context.Context) error {
	log := p.log()

	// 1. Resolve the tracked sources for this run, wrap each in taggedSource
	// (stamps SourceID/TopicID onto a COPY of the collected items, §4.2) and
	// collect. Partial source errors are non-fatal (feed.Collect semantics).
	tracked, err := p.SourceProvider(ctx)
	if err != nil {
		return fmt.Errorf("resolve sources: %w", err)
	}
	sources := make([]feed.Source, 0, len(tracked))
	for _, t := range tracked {
		sources = append(sources, taggedSource{
			inner: t.Src, sourceID: t.SourceID, topicID: t.TopicID,
		})
	}
	items, collectErr := feed.Collect(ctx, sources)
	if collectErr != nil {
		log.Warn("partial source errors", "err", collectErr)
	}

	// 2. Drop items already delivered in a previous run; nothing new — exit
	// quietly (no TOC, no digests rows).
	fresh, err := p.Store.FilterUnseen(ctx, items)
	if err != nil {
		return fmt.Errorf("filter unseen: %w", err)
	}
	if len(fresh) == 0 {
		log.Info("no new items, skipping digest")
		return nil
	}

	// 3. Resolve topic names ONCE and group the fresh items by TopicID
	// (0 → 1 «Без темы»). Topics are ordered by id asc; items inside a topic
	// keep collect order (stable — no map iteration in the output).
	topics, err := p.Store.ListTopics(ctx)
	if err != nil {
		return fmt.Errorf("list topics: %w", err)
	}
	topicNames := make(map[int64]string, len(topics))
	for _, t := range topics {
		topicNames[t.ID] = t.Name
	}
	groups := make(map[int64][]feed.Item)
	var topicIDs []int64
	for _, it := range fresh {
		tid := it.TopicID
		if tid == 0 {
			tid = 1 // «Без темы»
		}
		if _, seen := groups[tid]; !seen {
			topicIDs = append(topicIDs, tid)
		}
		groups[tid] = append(groups[tid], it)
	}
	slices.Sort(topicIDs)

	// 4. One runID for the whole run (collision within a second is practically
	// impossible for a single-user service with LLM calls; tests fix Now).
	now := time.Now()
	if p.Now != nil {
		now = p.Now()
	}
	runID := now.Unix()

	// 5. Per topic: summarize → render the L1 body → InsertDigest(delivered=0).
	// A failed topic is SKIPPED: no digests row, its items stay unseen and are
	// picked up by the next run.
	var pending []pendingTopic
	entries := make([]TOCEntry, 0, len(topicIDs))
	for _, tid := range topicIDs {
		topicItems := groups[tid]
		name, ok := topicNames[tid]
		if !ok {
			// FK is not enforced — a dangling topic_id must not kill the run.
			name = fmt.Sprintf("Тема #%d", tid)
		}
		ts, err := p.TopicSummary.SummarizeTopic(ctx, name, topicItems)
		if err != nil {
			log.Error("topic summarize failed; topic skipped",
				"topic_id", tid, "items", len(topicItems), "err", err)
			continue
		}
		// Fallback titles (C1 отклонение 2): with an empty Headline the body
		// renders «• <title>» bullets of the first items — pass them, or the
		// fallback branch is dead in production.
		limit := min(len(topicItems), 10)
		fallbackTitles := make([]string, 0, limit)
		for _, it := range topicItems[:limit] {
			fallbackTitles = append(fallbackTitles, it.Title)
		}
		body := RenderTopicBody(name, ts.Headline, len(topicItems), now, fallbackTitles...)

		digestID, err := p.Store.InsertDigest(ctx, runID, tid, body, len(topicItems))
		if err != nil {
			log.Error("insert digest failed; topic skipped", "topic_id", tid, "err", err)
			continue
		}
		summaries := make(map[int]string, len(ts.Items))
		for _, is := range ts.Items {
			summaries[is.Index] = is.Summary
		}
		ditems := make([]storage.DigestItem, 0, len(topicItems))
		for i, it := range topicItems {
			ditems = append(ditems, storage.NewDigestItem(it, summaries[i+1]))
		}
		pending = append(pending, pendingTopic{digestID: digestID, items: ditems})
		entries = append(entries, TOCEntry{
			DigestID: digestID, TopicName: name, ItemCount: len(topicItems),
		})
	}

	// 6. Fresh items existed but EVERY topic failed → run error (today's
	// semantics: fresh + summarize failure = failed run; §4.9). The scheduler
	// slot stays unmarked (runJob sets it only on success).
	if len(entries) == 0 {
		return fmt.Errorf("digest: all %d topics failed", len(topicIDs))
	}

	// 7. Deliver ONE TOC message with per-topic «Открыть» buttons. Delivery
	// error → run error: all rows stay delivered=0, items unseen, slot
	// unmarked — the next run rebuilds the TOC.
	text := RenderTOC(entries, now.Format("02 Jan 2006"))
	buttons := TOCButtons(entries)
	if err := p.Poster.DeliverWithButtons(ctx, p.ChatID, text, buttons); err != nil {
		return fmt.Errorf("deliver toc: %w", err)
	}

	// 8. Flip delivered=1 + write digest_items + mark seen, one transaction
	// per topic. Errors are log-only: the TOC already reached the user; an
	// affected topic's items stay unseen and duplicate into the next run
	// (accepted scenario, §4.9 / plan §C step 8).
	for _, pt := range pending {
		if err := p.Store.MarkDelivered(ctx, pt.digestID, pt.items); err != nil {
			log.Error("mark delivered failed", "digest_id", pt.digestID, "err", err)
		}
	}

	// 9.
	log.Info("digest delivered", "topics", len(entries), "run_id", runID)
	return nil
}

// TrackedSource couples a feed.Source with its storage identity (Stage 8 C,
// plan §4.2). SourceID 0 = no DB row (e.g. a static source that failed to
// upsert); TopicID 0 → «Без темы» (topic 1).
type TrackedSource struct {
	Src      feed.Source
	SourceID int64
	TopicID  int64
}

// taggedSource wraps a feed.Source and stamps SourceID/TopicID onto a COPY of
// the collected items (§4.2). The originals are never mutated: static sources
// are reused across runs. The inner error is returned as-is together with the
// (possibly partial) stamped items; NOTE that feed.Collect itself is
// sequential and DROPS a failed source's items (it appends the error and
// continues) — partial items are never consumed in the production path. No
// data is lost: undelivered items stay unseen and the next run picks them up.
type taggedSource struct {
	inner             feed.Source
	sourceID, topicID int64
}

// Name delegates to the inner source.
func (t taggedSource) Name() string { return t.inner.Name() }

// Collect stamps provenance onto a copy of the inner items.
func (t taggedSource) Collect(ctx context.Context) ([]feed.Item, error) {
	items, err := t.inner.Collect(ctx)
	stamped := make([]feed.Item, len(items))
	copy(stamped, items)
	for i := range stamped {
		stamped[i].SourceID = t.sourceID
		stamped[i].TopicID = t.topicID
	}
	return stamped, err
}
