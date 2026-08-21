package bravesearch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents"
)

const sampleResponse = `{
  "type": "search",
  "web": {
    "results": [
      {"title": "Go Programming Language", "url": "https://go.dev", "description": "The <strong>Go</strong> language."},
      {"title": "Effective Go", "url": "https://go.dev/doc/effective_go", "description": "Tips for writing Go."}
    ]
  }
}`

func TestNewRequiresAPIKey(t *testing.T) {
	t.Setenv("BRAVE_API_KEY", "")
	if _, err := New(Options{}); err == nil {
		t.Fatal("expected error when no API key is set")
	}
	if _, err := New(Options{APIKey: "k"}); err != nil {
		t.Fatalf("unexpected error with explicit key: %v", err)
	}
}

func TestNewUsesEnvAPIKey(t *testing.T) {
	t.Setenv("BRAVE_API_KEY", "from-env")
	if _, err := New(Options{}); err != nil {
		t.Fatalf("expected env key to satisfy New: %v", err)
	}
}

func TestSearchSendsRequestAndFormats(t *testing.T) {
	var gotToken, gotQuery, gotCount string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Subscription-Token")
		gotQuery = r.URL.Query().Get("q")
		gotCount = r.URL.Query().Get("count")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(sampleResponse))
	}))
	defer srv.Close()

	tool, err := New(Options{APIKey: "secret", Count: 3, Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}

	out, err := tool.OnInvoke(context.Background(), &agents.ToolContext{}, `{"query":"golang"}`)
	if err != nil {
		t.Fatalf("OnInvoke: %v", err)
	}

	if gotToken != "secret" {
		t.Errorf("token header = %q, want %q", gotToken, "secret")
	}
	if gotQuery != "golang" {
		t.Errorf("q = %q, want %q", gotQuery, "golang")
	}
	if gotCount != "3" {
		t.Errorf("count = %q, want %q", gotCount, "3")
	}

	text, ok := out.ModelOutput().(string)
	if !ok {
		t.Fatalf("output type = %T, want string", out)
	}
	for _, want := range []string{"Go Programming Language", "https://go.dev", "The Go language.", "Effective Go"} {
		if !strings.Contains(text, want) {
			t.Errorf("output missing %q\n--- got ---\n%s", want, text)
		}
	}
	if strings.Contains(text, "<strong>") {
		t.Errorf("highlight markers not stripped:\n%s", text)
	}
}

func TestCountClampedToMax(t *testing.T) {
	var gotCount string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCount = r.URL.Query().Get("count")
		_, _ = w.Write([]byte(`{"web":{"results":[]}}`))
	}))
	defer srv.Close()

	tool, err := New(Options{APIKey: "k", Count: 999, Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tool.OnInvoke(context.Background(), &agents.ToolContext{}, `{"query":"x"}`); err != nil {
		t.Fatal(err)
	}
	if gotCount != "20" {
		t.Errorf("count = %q, want clamped to 20", gotCount)
	}
}

func TestNoResults(t *testing.T) {
	out, err := formatResults([]byte(`{"web":{"results":[]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if out != "No results found." {
		t.Errorf("got %q", out)
	}

	out, err = formatResults([]byte(`{"type":"search"}`))
	if err != nil {
		t.Fatal(err)
	}
	if out != "No results found." {
		t.Errorf("nil web: got %q", out)
	}
}

func TestAPIErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate limited"}`))
	}))
	defer srv.Close()

	tool, err := New(Options{APIKey: "k", Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = tool.OnInvoke(context.Background(), &agents.ToolContext{}, `{"query":"x"}`)
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("expected 429 error, got %v", err)
	}
}

func TestEmptyQueryRejected(t *testing.T) {
	tool, err := New(Options{APIKey: "k", Endpoint: "http://unused"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tool.OnInvoke(context.Background(), &agents.ToolContext{}, `{"query":"  "}`); err == nil {
		t.Fatal("expected error for blank query")
	}
}
