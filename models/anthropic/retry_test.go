package anthropic

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	ant "github.com/anthropics/anthropic-sdk-go"
)

func apiError(status int, header http.Header) *ant.Error {
	if header == nil {
		header = http.Header{}
	}
	return &ant.Error{StatusCode: status, Response: &http.Response{Header: header}}
}

func TestRetryableError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"canceled", context.Canceled, false},
		{"deadline", context.DeadlineExceeded, true},
		{"rate_limited", apiError(429, nil), true},
		{"overloaded_529", apiError(529, nil), true},
		{"server_error", apiError(500, nil), true},
		{"bad_request", apiError(400, nil), false},
		{"auth", apiError(401, nil), false},
		{"should_retry_overrides_400", apiError(400, http.Header{"X-Should-Retry": []string{"true"}}), true},
		{"should_not_retry_overrides_500", apiError(500, http.Header{"X-Should-Retry": []string{"false"}}), false},
		{"wrapped", fmt.Errorf("anthropic messages: %w", apiError(429, nil)), true},
	} {
		if got := RetryableError(tc.err); got != tc.want {
			t.Errorf("%s: RetryableError = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestRetryAfter(t *testing.T) {
	d, ok := RetryAfter(apiError(429, http.Header{"Retry-After": []string{"2"}}))
	if !ok || d != 2*time.Second {
		t.Errorf("RetryAfter = %v/%v, want 2s/true", d, ok)
	}
	if _, ok := RetryAfter(apiError(429, nil)); ok {
		t.Error("RetryAfter without header should report false")
	}
	if _, ok := RetryAfter(errors.New("plain")); ok {
		t.Error("RetryAfter on a non-API error should report false")
	}
}
