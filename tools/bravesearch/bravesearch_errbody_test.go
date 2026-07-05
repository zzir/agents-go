package bravesearch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents"
)

// TestAPIErrorBodyTruncated pins that a huge error response body is cut to
// ~1 KiB before being folded into the error fed back to the model, instead of
// shipping up to 4 MiB of body verbatim.
func TestAPIErrorBodyTruncated(t *testing.T) {
	big := strings.Repeat("e", 64<<10) // 64 KiB error body
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(big))
	}))
	defer srv.Close()

	tool, err := New(Options{APIKey: "k", Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = tool.OnInvoke(context.Background(), &agents.ToolContext{}, `{"query":"x"}`)
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	msg := err.Error()
	if len(msg) > 2048 {
		t.Errorf("error message is %d bytes; the response body must be truncated to ~1 KiB", len(msg))
	}
	for _, want := range []string{"500", "[... truncated]"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q (prefix: %q)", want, msg[:80])
		}
	}
}
