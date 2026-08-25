package anthropic

import (
	"errors"
	"net/http"
	"time"

	ant "github.com/anthropics/anthropic-sdk-go"

	"github.com/zzir/agents-go/models/modelkit"
)

// unwrapAPIError extracts the anthropic-sdk-go API error from err's chain, for
// the shared modelkit retry classification.
func unwrapAPIError(err error) (int, http.Header, bool) {
	apiErr, ok := errors.AsType[*ant.Error](err)
	if !ok {
		return 0, nil, false
	}
	var h http.Header
	if apiErr.Response != nil {
		h = apiErr.Response.Header
	}
	return apiErr.StatusCode, h, true
}

// RetryableError reports whether err from a Messages API call is transient and
// worth retrying: HTTP 408/409/429 and any 5xx — including Anthropic's 529
// overloaded_error — with an explicit X-Should-Retry header outranking the
// status, plus network-level transport errors; never context cancellation. See
// modelkit.RetryableError for the full rules.
//
// Use it as agents.RetryPolicy.RetryIf:
//
//	policy := agents.RetryPolicy{RetryIf: anthropic.RetryableError, RetryAfter: anthropic.RetryAfter}
func RetryableError(err error) bool {
	return modelkit.RetryableError(err, unwrapAPIError)
}

// RetryAfter extracts a server-suggested delay from an error's Retry-After
// response header, for use as agents.RetryPolicy.RetryAfter.
func RetryAfter(err error) (time.Duration, bool) {
	return modelkit.RetryAfter(err, unwrapAPIError)
}
