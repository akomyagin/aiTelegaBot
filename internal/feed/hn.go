package feed

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// HNSource collects top stories from the Hacker News Firebase API.
type HNSource struct {
	name    string
	baseURL string // default HN Firebase v0 root; override for tests
	limit   int
	http    *http.Client
	log     *slog.Logger
}

// NewHNSource builds a Hacker News source fetching at most limit top stories.
func NewHNSource(name string, limit int, hc *http.Client) *HNSource {
	return &HNSource{
		name:    name,
		baseURL: "https://hacker-news.firebaseio.com/v0",
		limit:   limit,
		http:    hc,
		log:     slog.Default(),
	}
}

// Name returns the human-readable source name.
func (s *HNSource) Name() string { return s.name }

// SetBaseURL overrides the HN API root (used by tests against httptest).
func (s *HNSource) SetBaseURL(u string) { s.baseURL = u }

type hnItem struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	URL   string `json:"url"`
	Text  string `json:"text"` // for Ask HN
	Time  int64  `json:"time"`
	Type  string `json:"type"`
	Dead  bool   `json:"dead"`
}

// Collect fetches the top stories, resolves each item, and normalizes them.
func (s *HNSource) Collect(ctx context.Context) ([]Item, error) {
	var ids []int64
	if err := getJSON(ctx, s.http, s.baseURL+"/topstories.json", &ids); err != nil {
		return nil, fmt.Errorf("topstories: %w", err)
	}
	if len(ids) > s.limit {
		ids = ids[:s.limit]
	}

	items := make([]Item, 0, len(ids))
	for _, id := range ids {
		var hi hnItem
		url := fmt.Sprintf("%s/item/%d.json", s.baseURL, id)
		if err := getJSON(ctx, s.http, url, &hi); err != nil {
			s.log.Warn("hn: skip item", "id", id, "err", err)
			continue
		}
		if hi.Type != "story" || hi.Dead || hi.Title == "" {
			continue
		}
		itemURL := hi.URL
		if itemURL == "" {
			itemURL = fmt.Sprintf("https://news.ycombinator.com/item?id=%d", hi.ID)
		}
		items = append(items, Item{
			Source:    s.name,
			Kind:      "hn",
			Title:     hi.Title,
			URL:       itemURL,
			Text:      hi.Text,
			Published: time.Unix(hi.Time, 0).UTC(),
		})
	}
	return items, nil
}

// getJSON performs a context-aware GET and decodes a JSON body into v.
func getJSON(ctx context.Context, client *http.Client, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("get %q: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("get %q: status %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return fmt.Errorf("decode %q: %w", url, err)
	}
	return nil
}
