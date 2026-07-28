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
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		// A deadline is usually the ATTEMPT's (option.WithRequestTimeout, a
		// per-call budget) — the hung-request case retrying exists for. When
		// it is the caller's own context that expired, the retry loop's next
		// wait sees ctx.Err() and stops anyway, so treating timeouts as
		// non-retryable only ever threw away attempts that had budget left.
		return true
	}
	var apiErr *oai.Error
	if errors.As(err, &apiErr) {
		// An explicit x-should-retry header outranks the status code: the
		// server knows whether *this* failure is transient, and a 500 it will
		// never recover from is as real as a 400 it would.
		if v, ok := shouldRetryHeader(apiErr); ok {
			return v
		}
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

// RetryAfter extracts a server-suggested delay from an error's response
// headers, for use as agents.RetryPolicy.RetryAfter. Matching openai-go's own
// retry logic, headers are consulted in order of preference: Retry-After-Ms
// (milliseconds, integer or float) first, then Retry-After (seconds as an
// integer or float, or an HTTP-date). It reports ok=false when no usable
// header is present, leaving the caller's computed backoff in effect.
func RetryAfter(err error) (time.Duration, bool) {
	var apiErr *oai.Error
	if !errors.As(err, &apiErr) || apiErr.Response == nil {
		return 0, false
	}
	header := apiErr.Response.Header
	if v := header.Get("Retry-After-Ms"); v != "" {
		if ms, e := strconv.ParseFloat(v, 64); e == nil && ms >= 0 {
			return time.Duration(ms * float64(time.Millisecond)), true
		}
	}
	v := header.Get("Retry-After")
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

// shouldRetryHeader reads the x-should-retry hint, which a provider sets to
// override what its status code would otherwise imply. Only the two exact
// values carry meaning; anything else is treated as absent so a malformed
// header falls back to status classification rather than disabling retries.
func shouldRetryHeader(apiErr *oai.Error) (bool, bool) {
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
