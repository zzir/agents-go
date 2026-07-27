package openai

import (
	"net/http"
	"testing"
	"time"

	oai "github.com/openai/openai-go/v3"
)

// x-should-retry outranks the status code: the server knows whether *this*
// failure is transient, and a 500 it will never recover from is as final as a
// 400 it would.
func TestRetryableErrorHonorsShouldRetryHeader(t *testing.T) {
	mk := func(status int, header string) error {
		e := &oai.Error{StatusCode: status, Response: &http.Response{Header: http.Header{}}}
		if header != "" {
			e.Response.Header.Set("X-Should-Retry", header)
		}
		return e
	}
	cases := []struct {
		name   string
		status int
		header string
		want   bool
	}{
		{"500 without header retries", 500, "", true},
		{"400 without header does not", 400, "", false},
		{"500 with false does not", 500, "false", false},
		{"400 with true does", 400, "true", true},
		{"429 with false does not", 429, "false", false},
		// A malformed value must fall back to status classification rather
		// than silently disabling retries.
		{"garbage header falls back (500)", 500, "yes-please", true},
		{"garbage header falls back (400)", 400, "yes-please", false},
		{"empty header falls back", 503, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RetryableError(mk(tc.status, tc.header)); got != tc.want {
				t.Errorf("RetryableError = %v, want %v", got, tc.want)
			}
		})
	}

	// No Response at all must not panic.
	if RetryableError(&oai.Error{StatusCode: 500}) != true {
		t.Error("a 500 with no response should still be retryable")
	}
}

// --- RetryAfter header parsing ----------------------------------------------

func TestRetryAfter_MsHeader(t *testing.T) {
	err := apiErr(http.StatusTooManyRequests, http.Header{"Retry-After-Ms": []string{"1500"}})
	d, ok := RetryAfter(err)
	if !ok || d != 1500*time.Millisecond {
		t.Fatalf("d=%v ok=%v, want 1.5s true", d, ok)
	}
}

func TestRetryAfter_MsPreferredOverSeconds(t *testing.T) {
	err := apiErr(http.StatusTooManyRequests, http.Header{
		"Retry-After-Ms": []string{"250"},
		"Retry-After":    []string{"7"},
	})
	d, ok := RetryAfter(err)
	if !ok || d != 250*time.Millisecond {
		t.Fatalf("d=%v ok=%v, want 250ms true (Retry-After-Ms wins)", d, ok)
	}
}

func TestRetryAfter_FloatSeconds(t *testing.T) {
	err := apiErr(http.StatusTooManyRequests, http.Header{"Retry-After": []string{"0.5"}})
	d, ok := RetryAfter(err)
	if !ok || d != 500*time.Millisecond {
		t.Fatalf("d=%v ok=%v, want 500ms true", d, ok)
	}
}

func TestRetryAfter_BadMsFallsThroughToSeconds(t *testing.T) {
	err := apiErr(http.StatusTooManyRequests, http.Header{
		"Retry-After-Ms": []string{"soon"},
		"Retry-After":    []string{"2"},
	})
	d, ok := RetryAfter(err)
	if !ok || d != 2*time.Second {
		t.Fatalf("d=%v ok=%v, want 2s true (unparseable ms header skipped)", d, ok)
	}
}
