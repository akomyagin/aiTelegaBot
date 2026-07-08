package llm

import (
	"strings"

	"github.com/akomyagin/aiTelegaBot/internal/feed"
)

// maxTextRunes caps how much of each item's body text is included in the prompt
// (in runes, not bytes, to avoid splitting multi-byte UTF-8 characters).
const maxTextRunes = 500

const systemPrompt = `Ты — ассистент, составляющий краткий русскоязычный дайджест новостей.
Для каждого материала напиши один пункт: заголовок, одно-два предложения сути и ссылку.
Формат: маркированный список, каждый пункт с новой строки.`

// message is a single chat message in an OpenAI-compatible request.
type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// buildPrompt builds the chat messages for the digest summarization request.
// The result is deterministic for a given input slice (order preserved): a
// fixed system message plus a user message that numbers the items in order.
func buildPrompt(items []feed.Item) []message {
	var b strings.Builder
	for i, it := range items {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(itoa(i + 1))
		b.WriteString(". ")
		b.WriteString(it.Title)
		b.WriteString("\nURL: ")
		b.WriteString(it.URL)
		if text := truncateRunes(it.Text, maxTextRunes); text != "" {
			b.WriteString("\n")
			b.WriteString(text)
		}
	}
	return []message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: b.String()},
	}
}

// truncateRunes returns s truncated to at most n runes.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// itoa formats a small non-negative int without importing strconv for a hot
// path; kept trivial and allocation-light.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
