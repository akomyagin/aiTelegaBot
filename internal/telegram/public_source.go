package telegram

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
)

// PublicSource scrapes https://t.me/s/<channel> for the latest posts of a
// public Telegram channel. It degrades gracefully: any error returns an empty
// slice and logs a warning rather than propagating, so a broken scrape never
// blocks the digest.
type PublicSource struct {
	name    string
	channel string // without @
	baseURL string // "https://t.me/s", override for tests
	http    *http.Client
	limit   int
	log     *slog.Logger
}

// NewPublicSource builds a best-effort scraper for a public Telegram channel.
func NewPublicSource(name, channel string, hc *http.Client, limit int) *PublicSource {
	return &PublicSource{
		name:    name,
		channel: channel,
		baseURL: "https://t.me/s",
		http:    hc,
		limit:   limit,
		log:     slog.Default(),
	}
}

// SetBaseURL overrides the base URL (for tests).
func (s *PublicSource) SetBaseURL(u string) { s.baseURL = u }

// Name returns the human-readable source name.
func (s *PublicSource) Name() string { return s.name }

// Collect scrapes the channel page. Any failure degrades to (nil, nil).
func (s *PublicSource) Collect(ctx context.Context) ([]feed.Item, error) {
	items, err := s.scrape(ctx)
	if err != nil {
		s.log.Warn("t.me/s scrape failed (degrading gracefully)", "channel", s.channel, "err", err)
		return nil, nil // graceful degradation — never fail the digest
	}
	return items, nil
}

func (s *PublicSource) scrape(ctx context.Context) ([]feed.Item, error) {
	url := fmt.Sprintf("%s/%s", s.baseURL, s.channel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; aiTelegaBot/1.0)")

	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	return parseChannelHTML(resp.Body, s.channel, s.limit)
}

// parseChannelHTML walks the t.me/s DOM extracting message posts into items.
func parseChannelHTML(r io.Reader, channel string, limit int) ([]feed.Item, error) {
	root, err := html.Parse(r)
	if err != nil {
		return nil, err
	}

	var items []feed.Item
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if limit > 0 && len(items) >= limit {
			return
		}
		if n.Type == html.ElementNode && n.Data == "div" &&
			hasClass(attr(n, "class"), "tgme_widget_message") {
			if item, ok := messageToItem(n, channel); ok {
				items = append(items, item)
			}
			// Do not descend into a matched message node.
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)

	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

// messageToItem extracts a single feed.Item from a tgme_widget_message node.
func messageToItem(msg *html.Node, channel string) (feed.Item, bool) {
	dataPost := attr(msg, "data-post")
	parts := strings.Split(dataPost, "/")
	if len(parts) < 2 || parts[1] == "" {
		return feed.Item{}, false
	}
	msgID := parts[1]

	text := findMessageText(msg)
	published := findMessageTime(msg)

	return feed.Item{
		Kind:      "tg_public",
		Source:    channel,
		ID:        msgID,
		Title:     firstLine(text),
		URL:       "https://t.me/" + channel + "/" + msgID,
		Text:      text,
		Published: published,
	}, true
}

// findMessageText returns the concatenated text of the first
// tgme_widget_message_text div within the node.
func findMessageText(n *html.Node) string {
	var found *html.Node
	var search func(*html.Node)
	search = func(node *html.Node) {
		if found != nil {
			return
		}
		if node.Type == html.ElementNode && node.Data == "div" &&
			hasClass(attr(node, "class"), "tgme_widget_message_text") {
			found = node
			return
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			search(c)
		}
	}
	search(n)
	if found == nil {
		return ""
	}
	return strings.TrimSpace(collectText(found))
}

// findMessageTime parses the datetime attribute of the first <time> element.
func findMessageTime(n *html.Node) time.Time {
	var result time.Time
	var found bool
	var search func(*html.Node)
	search = func(node *html.Node) {
		if found {
			return
		}
		if node.Type == html.ElementNode && node.Data == "time" {
			if dt := attr(node, "datetime"); dt != "" {
				if t, err := time.Parse(time.RFC3339, dt); err == nil {
					result = t
					found = true
					return
				}
			}
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			search(c)
		}
	}
	search(n)
	return result
}

// collectText concatenates all descendant text nodes, turning <br> into "\n".
func collectText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.TextNode {
			b.WriteString(node.Data)
			return
		}
		if node.Type == html.ElementNode && node.Data == "br" {
			b.WriteByte('\n')
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

// attr returns the value of the named attribute, or "" if absent.
func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// hasClass reports whether a space-separated class list contains cls.
func hasClass(classList, cls string) bool {
	for _, c := range strings.Fields(classList) {
		if c == cls {
			return true
		}
	}
	return false
}
