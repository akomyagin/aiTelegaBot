package llm

import (
	"context"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"time"
)

const (
	retryBaseDelay = 500 * time.Millisecond
	retryMaxDelay  = 30 * time.Second
	errBodyLimit   = 4 << 10 // 4KB cap on error response bodies
)

// doWithRetry executes an HTTP request with exponential backoff + jitter.
// Retryable errors are network errors and APIError{Retryable: true}. On a 200
// response the *http.Response is returned with its Body still open for the
// caller to read and close. When retries are exhausted the last error is
// returned.
func (c *Client) doWithRetry(ctx context.Context, makeReq func() (*http.Request, error)) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		req, err := makeReq()
		if err != nil {
			return nil, err
		}

		resp, err := c.http.Do(req)
		if err != nil {
			// Network error — retryable.
			lastErr = err
			c.log.Warn("llm request failed (network)", "attempt", attempt, "err", err)
			if attempt < c.maxRetries {
				if werr := c.wait(ctx, backoff(attempt)); werr != nil {
					return nil, werr
				}
				continue
			}
			return nil, lastErr
		}

		if resp.StatusCode == http.StatusOK {
			return resp, nil
		}

		body, _ := io.ReadAll(io.LimitReader(resp.Body, errBodyLimit))
		_ = resp.Body.Close()
		apiErr := &APIError{
			StatusCode: resp.StatusCode,
			Retryable:  classify(resp.StatusCode),
			Msg:        string(body),
		}
		lastErr = apiErr

		if !apiErr.Retryable {
			return nil, apiErr
		}
		c.log.Warn("llm request failed (retryable)", "attempt", attempt, "status", resp.StatusCode)
		if attempt < c.maxRetries {
			delay := backoff(attempt)
			if ra := retryAfter(resp.Header); ra > 0 {
				delay = ra
			}
			if werr := c.wait(ctx, delay); werr != nil {
				return nil, werr
			}
			continue
		}
		return nil, lastErr
	}
	return nil, lastErr
}

// wait sleeps for d or returns early if ctx is cancelled.
func (c *Client) wait(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// backoff returns base*2^attempt capped at retryMaxDelay, plus jitter in
// [0, d/2).
func backoff(attempt int) time.Duration {
	d := retryBaseDelay << attempt
	if d <= 0 || d > retryMaxDelay {
		d = retryMaxDelay
	}
	jitter := time.Duration(rand.Int63n(int64(d / 2)))
	return d + jitter
}

// retryAfter parses the Retry-After header as an integer number of seconds.
// Returns 0 when absent or unparseable.
func retryAfter(h http.Header) time.Duration {
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}
