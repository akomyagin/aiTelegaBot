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
		item := Item{
			Source:    s.name,
			Kind:      s.kind,
			Title:     strings.TrimSpace(fi.Title),
			URL:       fi.Link,
			Text:      text,
			Published: published,
		}
		// Медиа (plan §4.5, Stage 9E): первая картинка элемента — enclosure
		// image/* или media:content (gofeed fi.Image).
		if mediaURL := mediaImageURL(fi); mediaURL != "" {
			item.MediaKind = "photo"
			item.MediaURL = mediaURL
		}
		items = append(items, item)
	}
	return items, nil
}

// mediaImageURL возвращает URL первой картинки gofeed-элемента (plan §4.5):
// enclosure с типом image/*, иначе fi.Image (media:content). Не-image
// enclosures (application/pdf и т.п.) игнорируются — документы/видео не несём
// (plan §2). Пустой Type у enclosure картинкой не считается. Принимаются
// только http/https URL (httpMediaURL) — прочее → "".
func mediaImageURL(fi *gofeed.Item) string {
	for _, e := range fi.Enclosures {
		if e != nil && e.URL != "" && strings.HasPrefix(e.Type, "image/") {
			return httpMediaURL(e.URL)
		}
	}
	if fi.Image != nil {
		return httpMediaURL(fi.Image.URL)
	}
	return ""
}

// httpMediaURL — gate схемы media-URL (plan §6, паттерн digest.l3Link):
// только http:// и https:// (без учёта регистра) — фото по URL качают серверы
// Telegram; file://, ftp:// и прочее отвергается → "" (media-поля источника
// не заполняются). Свой хелпер в пакете feed: feed НЕ импортирует telegram
// (направление зависимостей, ревью 9E фикс 2).
func httpMediaURL(u string) string {
	lower := strings.ToLower(u)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return u
	}
	return ""
}
