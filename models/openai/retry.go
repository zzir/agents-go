package openai

import (
	"net/http"
	"time"

	oai "github.com/openai/openai-go/v3"

	"github.com/zzir/agents-go/models/modelkit"
)

// unwrapAPIError reads the openai-go API error for the shared modelkit retry
// classification.
var unwrapAPIError = modelkit.UnwrapAs(func(e *oai.Error) (int, http.Header) {
	if e.Response == nil {
		return e.StatusCode, nil
	}
	return e.StatusCode, e.Response.Header
})

// RetryableError reports whether err from a Responses API call is transient
// and worth retrying, by modelkit.RetryableError's rules.
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
