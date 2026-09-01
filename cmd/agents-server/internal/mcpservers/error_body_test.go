package mcpservers

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The error-body logger sniffs a prefix of an error response to log it; the
// caller must still read the complete body, including the part past the sniff
// limit.
func TestErrorBodyRoundTripperPreservesBody(t *testing.T) {
	long := strings.Repeat("x", 5000) // past the 2KB sniff limit
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, long)
	}))
	t.Cleanup(srv.Close)

	resp, err := httpClientFor(nil, nil).Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(body) != long {
		t.Fatalf("body after sniffing = %d bytes, want %d unchanged", len(body), len(long))
	}
}
