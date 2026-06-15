package openai

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"

	oai "github.com/openai/openai-go/v3"
)

// RetryableError reports whether err from a Responses API call is transient and
// worth retrying. It returns true for HTTP 408 (request timeout), 409 (conflict),
// 429 (rate limit) and any 5xx, and for network-level transport errors. It
// returns false for context cancellation and for other 4xx client errors, which
// will not succeed on retry.
//
// Use it as agents.RetryPolicy.RetryIf:
//
//	policy := agents.RetryPolicy{RetryIf: openai.RetryableError, RetryAfter: openai.RetryAfter}
func RetryableError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var apiErr *oai.Error
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests:
			return true
		}
		return apiErr.StatusCode >= 500 && apiErr.StatusCode <= 599
	}
	// Connection/transport failures are not wrapped as *oai.Error; treat network
	// errors as retryable.
	var netErr net.Error
	return errors.As(err, &netErr)
}

// RetryAfter extracts a server-suggested delay from an error's Retry-After
// response header (seconds or HTTP-date form), for use as
// agents.RetryPolicy.RetryAfter. It reports ok=false when no usable header is
// present, leaving the caller's computed backoff in effect.
func RetryAfter(err error) (time.Duration, bool) {
	var apiErr *oai.Error
	if !errors.As(err, &apiErr) || apiErr.Response == nil {
		return 0, false
	}
	v := apiErr.Response.Header.Get("Retry-After")
	if v == "" {
		return 0, false
	}
	if secs, e := strconv.Atoi(v); e == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, e := http.ParseTime(v); e == nil {
		if d := time.Until(t); d > 0 {
			return d, true
		}
	}
	return 0, false
}
