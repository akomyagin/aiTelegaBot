// Package linkfetch downloads external web pages linked from Telegram
// messages and extracts plain text for LLM summarization (Stage 9B, plan
// §4.3).
//
// Ethics and limits (plan §2, planner decision — no robots.txt parsing):
// at most one GET per URL and NO retries ever (including 429/503), at most
// 3 redirect hops, a polite bot User-Agent, and the response body is capped
// via io.LimitReader so oversized pages never blow up RAM.
//
// Any failure — HTTP error status, unsupported Content-Type, network error,
// too many redirects, parse error — is returned as a plain error. The digest
// pipeline treats every error uniformly as kind=unavailable, so no typed
// error classification is needed here.
package linkfetch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/html"
)

const (
	// fetchTextRunes caps the extracted page text in runes (not bytes) so a
	// truncation never splits a UTF-8 character (plan §4.3: ≤6000 runes).
	fetchTextRunes = 6000

	// maxRedirects is the redirect hop limit: 3 hops are followed, the 4th
	// is an error (plan §2: «не следуем редиректам дольше 3 хоппов»).
	maxRedirects = 3

	// titleRunes caps the extracted <title>/<h1> text in runes (review F4):
	// an oversized title must not bloat link_cache.title nor the F prompt.
	// Truncation is on a rune boundary (truncateRunes).
	titleRunes = 300

	// defaultUserAgent matches telegram.PublicSource — a polite bot identity.
	defaultUserAgent = "Mozilla/5.0 (compatible; aiTelegaBot/1.0)"

	// defaultMaxBytes is used when NewFetcher receives maxBytes <= 0
	// (plan §4.3: LINK_FETCH_MAX_BYTES default 2 MB).
	defaultMaxBytes int64 = 2 << 20

	// defaultTimeout is used only when NewFetcher receives a nil *http.Client
	// (plan §4.3: LINK_FETCH_TIMEOUT default 15s). Callers normally own the
	// timeout via their http.Client and/or the per-Fetch context.
	defaultTimeout = 15 * time.Second
)

// skipTextTags lists elements whose subtree contributes nothing to the page
// text (plan §4.3 filter list). "title" is excluded from the text walk as
// well because it is extracted separately into Page.Title.
var skipTextTags = map[string]bool{
	"script":   true,
	"style":    true,
	"noscript": true,
	"svg":      true,
	"header":   true,
	"footer":   true,
	"nav":      true,
	"aside":    true,
	"form":     true,
	"title":    true,
}

// blockTags get a newline after their content so adjacent blocks are not
// glued into one run ("First""Second" → "First\nSecond").
var blockTags = map[string]bool{
	"html": true, "body": true, "main": true, "div": true, "p": true,
	"section": true, "article": true, "blockquote": true, "pre": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"ul": true, "ol": true, "li": true, "table": true, "tr": true,
	"figure": true, "figcaption": true, "dd": true, "dt": true,
}

// Page is the extracted content of one fetched URL.
type Page struct {
	URL   string // final URL after ≤3 redirects
	Title string // <title>, or the first <h1> when <title> is empty; may be ""
	Text  string // extracted text, ≤ fetchTextRunes runes
}

// Fetcher downloads a page with a single GET and extracts its text. It is a
// concrete type on purpose: no second implementation is foreseen, so no
// interface (SKILL.md: «интерфейс появляется на второй реализации»).
type Fetcher struct {
	hc        *http.Client
	maxBytes  int64
	userAgent string
}

// NewFetcher builds a Fetcher. hc may be nil (a client with a 15s timeout is
// created); otherwise hc is shallow-copied — the caller's client is never
// mutated — and the copy gets the ≤3-hop CheckRedirect policy. maxBytes <= 0
// means the 2 MB default.
func NewFetcher(hc *http.Client, maxBytes int64) *Fetcher {
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}
	base := &http.Client{Timeout: defaultTimeout}
	if hc != nil {
		clone := *hc
		base = &clone
	}
	base.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > maxRedirects {
			return fmt.Errorf("linkfetch: stopped after %d redirect hops (limit)", maxRedirects)
		}
		return nil
	}
	return &Fetcher{hc: base, maxBytes: maxBytes, userAgent: defaultUserAgent}
}

// Fetch performs exactly one GET (no retries by design — plan §2 ethics) and
// extracts the page text.
//
// Behavior contract:
//   - The body is read through io.LimitReader(resp.Body, maxBytes). An
//     oversized body is TRUNCATED, not rejected: html.Parse then parses the
//     first maxBytes bytes and the partial text is returned with a nil error.
//     Rationale: partial article text is still useful for summarization, and
//     the cap guarantees bounded RAM (458 MB VPS).
//   - Only Content-Type "text/html" (with any charset) or an empty
//     Content-Type is accepted; anything else (application/json,
//     application/pdf, text/plain, ...) is an error.
//   - Any non-2xx status (including 429/503) is an error without retries.
//   - Cancellation/timeout comes from ctx (and/or the http.Client's Timeout).
//
// Error messages contain the URL (not a secret per plan §6) but never any
// page body or credentials.
func (f *Fetcher) Fetch(ctx context.Context, url string) (Page, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Page{}, fmt.Errorf("linkfetch: build request for %s: %w", url, err)
	}
	req.Header.Set("User-Agent", f.userAgent)

	resp, err := f.hc.Do(req)
	if err != nil {
		return Page{}, fmt.Errorf("linkfetch: GET %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Page{}, fmt.Errorf("linkfetch: %s: unexpected status %d (no retry by design)", url, resp.StatusCode)
	}
	if ct := normalizeContentType(resp.Header.Get("Content-Type")); ct != "" && ct != "text/html" {
		return Page{}, fmt.Errorf("linkfetch: %s: unsupported content type %q (only text/html)", url, ct)
	}

	root, err := html.Parse(io.LimitReader(resp.Body, f.maxBytes))
	if err != nil {
		return Page{}, fmt.Errorf("linkfetch: %s: parse HTML: %w", url, err)
	}

	finalURL := url
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}
	return Page{
		URL:   finalURL,
		Title: extractTitle(root),
		Text:  truncateRunes(extractText(root), fetchTextRunes),
	}, nil
}

// normalizeContentType lowercases a Content-Type value and drops parameters
// ("text/html; charset=utf-8" → "text/html").
func normalizeContentType(v string) string {
	if i := strings.IndexByte(v, ';'); i >= 0 {
		v = v[:i]
	}
	return strings.ToLower(strings.TrimSpace(v))
}

// extractTitle returns the trimmed <title> text, falling back to the first
// <h1> when <title> is missing or blank. The result is capped at titleRunes
// runes (review F4). May return "".
func extractTitle(root *html.Node) string {
	if n := findFirstElement(root, "title"); n != nil {
		if t := strings.TrimSpace(extractText(n)); t != "" {
			return truncateRunes(t, titleRunes)
		}
	}
	if n := findFirstElement(root, "h1"); n != nil {
		return truncateRunes(strings.TrimSpace(extractText(n)), titleRunes)
	}
	return ""
}

// findFirstElement returns the first descendant-or-self element node with the
// given tag name, in document order.
func findFirstElement(root *html.Node, tag string) *html.Node {
	var found *html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if found != nil {
			return
		}
		if n.Type == html.ElementNode && n.Data == tag {
			found = n
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return found
}

// extractText concatenates the text under root, skipping skipTextTags
// subtrees entirely (root itself is never skipped, so extractText of a
// <title> or <h1> node works). <br> becomes "\n"; blockTags get a trailing
// "\n". Whitespace within lines is collapsed, empty lines are dropped.
// Pattern: collectText from internal/telegram/public_source.go, extended
// with the §4.3 filters.
func extractText(root *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if skipTextTags[n.Data] {
				return
			}
			if n.Data == "br" {
				b.WriteByte('\n')
				return
			}
		}
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == html.ElementNode && blockTags[n.Data] {
			b.WriteByte('\n')
		}
	}
	if root.Type == html.TextNode {
		b.WriteString(root.Data)
	} else {
		for c := root.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}

	lines := strings.Split(b.String(), "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		if ln = strings.Join(strings.Fields(ln), " "); ln != "" {
			out = append(out, ln)
		}
	}
	return strings.Join(out, "\n")
}

// truncateRunes cuts s to at most max runes — on a rune boundary, never
// mid-character.
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
