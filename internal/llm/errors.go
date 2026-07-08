package llm

import "fmt"

// APIError is a typed error from the LLM API. Msg never contains the API key.
type APIError struct {
	StatusCode int
	Retryable  bool
	Msg        string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("llm api error %d: %s", e.StatusCode, e.Msg)
}

// classify reports whether an HTTP status code is retryable.
// 429, 408 and 5xx are retryable; other 4xx are fatal. Network errors are
// handled separately (always retryable) in doWithRetry.
func classify(statusCode int) (retryable bool) {
	switch {
	case statusCode == 429:
		return true
	case statusCode == 408:
		return true
	case statusCode >= 500:
		return true
	default:
		return false
	}
}
