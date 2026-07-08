package feed

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/mmcdole/gofeed"
)

// RSSSource collects items from an RSS/Atom feed (also used for arXiv Atom feeds).
type RSSSource struct {
	name string
	url  string
	kind string // "rss" or "arxiv"
	http *http.Client
	fp   *gofeed.Parser
}

// NewRSSSource builds an RSS/Atom source. kind is "rss" or "arxiv".
func NewRSSSource(name, url, kind string, hc *http.Client) *RSSSource {
	fp := gofeed.NewParser()
	if hc != nil {
		fp.Client = hc
	}
	return &RSSSource{name: name, url: url, kind: kind, http: hc, fp: fp}
}

// Name returns the human-readable source name.
func (s *RSSSource) Name() string { return s.name }

// Collect fetches and parses the feed into normalized Items.
func (s *RSSSource) Collect(ctx context.Context) ([]Item, error) {
	f, err := s.fp.ParseURLWithContext(s.url, ctx)
	if err != nil {
		return nil, fmt.Errorf("parse feed %q: %w", s.url, err)
	}

	items := make([]Item, 0, len(f.Items))
	for _, fi := range f.Items {
		if fi == nil {
			continue
		}
		text := fi.Description
		if text == "" {
			text = fi.Content
		}
		var published = timeZero
		if fi.PublishedParsed != nil {
			published = *fi.PublishedParsed
		} else if fi.UpdatedParsed != nil {
			published = *fi.UpdatedParsed
		}
		items = append(items, Item{
			Source:    s.name,
			Kind:      s.kind,
			Title:     strings.TrimSpace(fi.Title),
			URL:       fi.Link,
			Text:      text,
			Published: published,
		})
	}
	return items, nil
}
