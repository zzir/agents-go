package modelkit

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
)

// UnwrapAPIError extracts a provider's API error from err's chain as its HTTP
// status code and response headers. Each backend adapter supplies one — they
// differ only in the SDK error type they errors.As for — and passes it to
// RetryableError / RetryAfter, which hold the shared classification.
type UnwrapAPIError func(err error) (statusCode int, header http.Header, ok bool)

// RetryableError reports whether a model-call error is transient and worth
// retrying. It is the shared half of each adapter's RetryableError.
//
// context.Canceled is never retried. context.DeadlineExceeded is — a deadline
// is usually the ATTEMPT's own budget (a per-call request timeout), the
// hung-request case retrying exists for; when it is the caller's own context
// that expired, the retry loop's next wait sees ctx.Err() and stops anyway,
// so treating timeouts as non-retryable only ever threw away attempts that
// had budget left. For API errors an explicit X-Should-Retry header outranks
// the status code — the server knows whether THIS failure is transient, and a
// 500 it will never recover from is as real as a 400 it would; only the two
// exact values carry meaning, so a malformed header falls back to status
// classification. Then 408 (request timeout), 409 (conflict), 429 (rate
// limit) and any 5xx are transient; other 4xx client errors will not succeed
// on retry. Errors the unwrap does not recognize are retryable only when they
// are network-level transport failures: net.Error, or io.ErrUnexpectedEOF —
// an SSE stream whose connection a gateway or proxy severed mid-flight, which
// reaches here as a plain io error rather than a net.Error.
func RetryableError(err error, unwrap UnwrapAPIError) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if status, header, ok := unwrap(err); ok {
		switch header.Get("X-Should-Retry") {
		case "true":
			return true
		case "false":
			return false
		}
		switch status {
		case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests:
			return true
		}
		return status >= 500 && status <= 599
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	_, isNet := errors.AsType[net.Error](err)
	return isNet
}

// RetryAfter extracts a server-suggested delay from an API error's response
// headers, for use as agents.RetryPolicy.RetryAfter. Headers are consulted in
// order of preference: Retry-After-Ms (milliseconds, integer or float — sent
// by OpenAI, absent elsewhere) first, then Retry-After (seconds as an integer
// or float, or an HTTP-date). It reports ok=false when no usable header is
// present, leaving the caller's computed backoff in effect.
func RetryAfter(err error, unwrap UnwrapAPIError) (time.Duration, bool) {
	_, header, ok := unwrap(err)
	if !ok || header == nil {
		return 0, false
	}
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
