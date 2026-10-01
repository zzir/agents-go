package anthropic

import (
	"errors"
	"net/http"
	"time"

	ant "github.com/anthropics/anthropic-sdk-go"

	"github.com/zzir/agents-go/models/modelkit"
)

// unwrapAPIError reads the anthropic-sdk-go API error for the shared modelkit
// retry classification.
var unwrapAPIError = modelkit.UnwrapAs(func(e *ant.Error) (int, http.Header) {
	if e.Response == nil {
		return e.StatusCode, nil
	}
	return e.StatusCode, e.Response.Header
})

// RetryableError reports whether err from a Messages API call is transient and
// worth retrying: HTTP 408/409/429 and any 5xx — including Anthropic's 529
// overloaded_error — with an explicit X-Should-Retry header outranking the
// status, plus network-level transport errors; never context cancellation. See
// modelkit.RetryableError for the full rules. An error event inside a 200
// stream is classified by its error type.
//
// Use it as agents.RetryPolicy.RetryIf:
//
//	policy := agents.RetryPolicy{RetryIf: anthropic.RetryableError, RetryAfter: anthropic.RetryAfter}
func RetryableError(err error) bool {
	if e, ok := errors.AsType[*ant.Error](err); ok && e.StatusCode < 300 {
		switch e.Type() {
		case ant.ErrorTypeOverloadedError, ant.ErrorTypeAPIError, ant.ErrorTypeRateLimitError, ant.ErrorTypeTimeoutError:
			return true
		}
		return false
	}
	return modelkit.RetryableError(err, unwrapAPIError)
}

// RetryAfter extracts a server-suggested delay from an error's Retry-After
// response header, for use as agents.RetryPolicy.RetryAfter.
func RetryAfter(err error) (time.Duration, bool) {
	return modelkit.RetryAfter(err, unwrapAPIError)
}
