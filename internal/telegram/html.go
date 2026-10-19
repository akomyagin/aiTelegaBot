package telegram

import "strings"

// htmlEscaper escapes the three characters that are special in Telegram's
// ParseMode HTML. Every dynamic text (channel titles, post headings, user
// data, error strings echoed back) MUST go through EscapeHTML before being
// embedded into a message; markup (<b>, <a href="…">) is added only by the
// render itself. Lesson of hotfix 4695043: raw angle brackets in a reply
// break delivery of the whole message.
var htmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// EscapeHTML escapes &, < and > for ParseMode HTML text.
func EscapeHTML(s string) string {
	return htmlEscaper.Replace(s)
}
