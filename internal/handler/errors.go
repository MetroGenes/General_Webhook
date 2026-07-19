package handler

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ErrNonRetryable marks a permanent action failure that must not be retried.
var ErrNonRetryable = errors.New("non-retryable")

// HTTPError carries HTTP status details for retry classification.
type HTTPError struct {
	Status     int
	Body       string
	RetryAfter time.Duration
}

func (e *HTTPError) Error() string {
	if e == nil {
		return "http error"
	}
	return fmt.Sprintf("http status %d: %s", e.Status, e.Body)
}

func (e *HTTPError) Is(target error) bool {
	return target == ErrNonRetryable && e != nil && !e.retryable()
}

func (e *HTTPError) retryable() bool {
	if e == nil {
		return false
	}
	switch e.Status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	default:
		return e.Status >= 500
	}
}

// IsNonRetryable reports whether err should not be retried.
func IsNonRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrNonRetryable) {
		return true
	}
	var he *HTTPError
	if errors.As(err, &he) {
		return !he.retryable()
	}
	return false
}

// RetryAfter extracts the server-provided Retry-After delay. The queue applies
// its configured retry-age deadline rather than silently shortening it here.
func RetryAfter(err error) time.Duration {
	var he *HTTPError
	if errors.As(err, &he) && he.RetryAfter > 0 {
		return he.RetryAfter
	}
	return 0
}

// ParseRetryAfter parses Retry-After header (seconds or HTTP-date).
func ParseRetryAfter(raw string, now time.Time) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if sec, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if sec <= 0 {
			return 0
		}
		const maxSeconds = int64(^uint64(0)>>1) / int64(time.Second)
		if sec > maxSeconds {
			return time.Duration(1<<63 - 1)
		}
		return time.Duration(sec) * time.Second
	}
	if t, err := http.ParseTime(raw); err == nil {
		d := t.Sub(now)
		if d <= 0 {
			return 0
		}
		return d
	}
	return 0
}
