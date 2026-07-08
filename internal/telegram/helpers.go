package telegram

import "strings"

// firstLine extracts the first non-empty line (up to 100 runes) as a title.
func firstLine(text string) string {
	text = strings.TrimSpace(text)
	if idx := strings.IndexByte(text, '\n'); idx >= 0 {
		text = text[:idx]
	}
	text = strings.TrimSpace(text)
	r := []rune(text)
	if len(r) > 100 {
		r = r[:100]
	}
	return string(r)
}
