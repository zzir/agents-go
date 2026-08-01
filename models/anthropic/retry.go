package anthropic

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"

	ant "github.com/anthropics/anthropic-sdk-go"
)

// RetryableError reports whether err from a Messages API call is transient and
// worth retrying. It returns true for HTTP 408, 409, 429 and any 5xx —
// including Anthropic's 529 overloaded_error — and for network-level
// transport errors. It returns false for context cancellation and other 4xx
// client errors, which will not succeed on retry.
//
// Use it as agents.RetryPolicy.RetryIf:
//
//	policy := agents.RetryPolicy{RetryIf: anthropic.RetryableError, RetryAfter: anthropic.RetryAfter}
func RetryableError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		// Usually the attempt's own budget (option.WithRequestTimeout); when it
		// is the caller's context, the retry loop's next wait sees ctx.Err()
		// and stops anyway. Same reasoning as the OpenAI provider.
		return true
	}
	var apiErr *ant.Error
	if errors.As(err, &apiErr) {
		// x-should-retry outranks the status code: the server knows whether
		// THIS failure is transient.
		if v, ok := shouldRetryHeader(apiErr); ok {
			return v
		}
		switch apiErr.StatusCode {
		case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests:
			return true
		}
		return apiErr.StatusCode >= 500 && apiErr.StatusCode <= 599
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

// RetryAfter extracts a server-suggested delay from an error's response
// headers, for use as agents.RetryPolicy.RetryAfter. The Messages API signals
// delay via Retry-After (seconds or an HTTP-date). It reports ok=false when no
// usable header is present, leaving the caller's computed backoff in effect.
func RetryAfter(err error) (time.Duration, bool) {
	var apiErr *ant.Error
	if !errors.As(err, &apiErr) || apiErr.Response == nil {
		return 0, false
	}
	v := apiErr.Response.Header.Get("Retry-After")
	if v == "" {
		return 0, false
	}
	if secs, e := strconv.ParseFloat(v, 64); e == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs * float64(time.Second)), true
	}
	if t, e := http.ParseTime(v); e == nil {
		if d := time.Until(t); d > 0 {
			return d, true
		}
	}
	return 0, false
}

// shouldRetryHeader reads the x-should-retry hint. Only the two exact values
// carry meaning; anything else falls back to status classification.
func shouldRetryHeader(apiErr *ant.Error) (bool, bool) {
	if apiErr.Response == nil {
		return false, false
	}
	switch apiErr.Response.Header.Get("X-Should-Retry") {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}
