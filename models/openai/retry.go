package openai

import (
	"errors"
	"net/http"
	"time"

	oai "github.com/openai/openai-go/v3"

	"github.com/zzir/agents-go/models/modelkit"
)

// unwrapAPIError extracts the openai-go API error from err's chain, for the
// shared modelkit retry classification.
func unwrapAPIError(err error) (int, http.Header, bool) {
	apiErr, ok := errors.AsType[*oai.Error](err)
	if !ok {
		return 0, nil, false
	}
	var h http.Header
	if apiErr.Response != nil {
		h = apiErr.Response.Header
	}
	return apiErr.StatusCode, h, true
}

// RetryableError reports whether err from a Responses API call is transient and
// worth retrying: HTTP 408/409/429 and any 5xx (with an explicit X-Should-Retry
// header outranking the status), plus network-level transport errors; never
// context cancellation. See modelkit.RetryableError for the full rules.
//
// Use it as agents.RetryPolicy.RetryIf:
//
//	policy := agents.RetryPolicy{RetryIf: openai.RetryableError, RetryAfter: openai.RetryAfter}
func RetryableError(err error) bool {
	return modelkit.RetryableError(err, unwrapAPIError)
}

// RetryAfter extracts a server-suggested delay from an error's response headers
// (Retry-After-Ms, then Retry-After), for use as agents.RetryPolicy.RetryAfter.
func RetryAfter(err error) (time.Duration, bool) {
	return modelkit.RetryAfter(err, unwrapAPIError)
}
