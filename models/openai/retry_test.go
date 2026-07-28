package openai

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	oai "github.com/openai/openai-go/v3"
)

func apiErr(status int, header http.Header) *oai.Error {
	return &oai.Error{StatusCode: status, Response: &http.Response{Header: header}}
}

type fakeNetErr struct{}

func (fakeNetErr) Error() string   { return "dial tcp: timeout" }
func (fakeNetErr) Timeout() bool   { return true }
func (fakeNetErr) Temporary() bool { return true }

func TestRetryableError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"429", apiErr(http.StatusTooManyRequests, nil), true},
		{"408", apiErr(http.StatusRequestTimeout, nil), true},
		{"409", apiErr(http.StatusConflict, nil), true},
		{"500", apiErr(http.StatusInternalServerError, nil), true},
		{"503", apiErr(http.StatusServiceUnavailable, nil), true},
		{"400", apiErr(http.StatusBadRequest, nil), false},
		{"401", apiErr(http.StatusUnauthorized, nil), false},
		{"404", apiErr(http.StatusNotFound, nil), false},
		{"wrapped-429", fmt.Errorf("call failed: %w", apiErr(http.StatusTooManyRequests, nil)), true},
		{"context-canceled", context.Canceled, false},
		// A deadline is usually the ATTEMPT's (a per-request timeout) — the
		// hung-call case retrying exists for. When the caller's own context
		// expired, the retry loop's next wait sees ctx.Err() and stops anyway.
		{"context-deadline", context.DeadlineExceeded, true},
		{"net-error", fakeNetErr{}, true},
		{"wrapped-net", fmt.Errorf("transport: %w", fakeNetErr{}), true},
		{"plain-error", errors.New("nope"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RetryableError(tc.err); got != tc.want {
				t.Errorf("RetryableError = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRetryAfter_Seconds(t *testing.T) {
	err := apiErr(http.StatusTooManyRequests, http.Header{"Retry-After": []string{"5"}})
	d, ok := RetryAfter(err)
	if !ok || d != 5*time.Second {
		t.Fatalf("d=%v ok=%v, want 5s true", d, ok)
	}
}

func TestRetryAfter_HTTPDate(t *testing.T) {
	future := time.Now().Add(2 * time.Minute).UTC().Format(http.TimeFormat)
	err := apiErr(http.StatusServiceUnavailable, http.Header{"Retry-After": []string{future}})
	d, ok := RetryAfter(err)
	if !ok || d <= 0 || d > 2*time.Minute+time.Second {
		t.Fatalf("d=%v ok=%v, want a positive sub-2m delay", d, ok)
	}
}

func TestRetryAfter_Absent(t *testing.T) {
	if _, ok := RetryAfter(apiErr(http.StatusTooManyRequests, http.Header{})); ok {
		t.Error("ok = true, want false for missing header")
	}
	if _, ok := RetryAfter(errors.New("not an api error")); ok {
		t.Error("ok = true, want false for non-API error")
	}
}

var _ net.Error = fakeNetErr{}
