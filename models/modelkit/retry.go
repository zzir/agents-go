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
// status code and response headers, for RetryableError / RetryAfter, which
// hold the shared classification. Build one with UnwrapAs.
type UnwrapAPIError func(err error) (statusCode int, header http.Header, ok bool)

// UnwrapAs builds the UnwrapAPIError for one SDK's error type E: errors.As
// finds it in the chain, and status reads its code and headers off it.
func UnwrapAs[E error](status func(E) (int, http.Header)) UnwrapAPIError {
	return func(err error) (int, http.Header, bool) {
		e, ok := errors.AsType[E](err)
		if !ok {
			return 0, nil, false
		}
		code, h := status(e)
		return code, h, true
	}
}

// RetryableError reports whether a model-call error is transient — the shared
// half of each adapter's RetryableError. context.Canceled never retries;
// context.DeadlineExceeded does. For an API error an exact X-Should-Retry
// "true"/"false" header outranks the status; otherwise 408, 409, 429 and any
// 5xx retry and other 4xx do not. An error the unwrap does not recognize
// retries only as a transport failure: a net.Error or io.ErrUnexpectedEOF.
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

// RetryAfter extracts a server-suggested delay from an API error's headers, for
// agents.RetryPolicy.RetryAfter: Retry-After-Ms (integer or float ms) first,
// then Retry-After (seconds, or an HTTP-date). ok=false when no usable header
// is present, leaving the caller's computed backoff in effect.
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
