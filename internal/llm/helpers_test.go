// Общие тест-хелперы пакета llm. chatBody раньше жил в topicdigest_test.go
// (удалён в 9D-2b вместе с TopicSummarizer — пункт E); вынесен сюда, т.к.
// используется тестами вызовов C/D (aggregate_test.go) и E/F (links_test.go).
package llm

import "strconv"

// chatBody wraps content into an OpenAI-compatible chat-completions response.
func chatBody(content string) string {
	return `{"choices":[{"message":{"content":` + strconv.Quote(content) + `}}]}`
}
