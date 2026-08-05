package openai

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/openai/openai-go/v3/option"

	"github.com/zzir/agents-go/agents"
)

// TestConversationsSession_AddItemsPartialFailureReported verifies that when a
// later batch fails (the API commits batches independently and cannot roll
// back), AddItems surfaces how much was already written so the caller can tell
// the server-side conversation is left holding a partial turn.
func TestConversationsSession_AddItemsPartialFailureReported(t *testing.T) {
	ctx := t.Context()
	var itemCalls atomic.Int64

	mux := http.NewServeMux()
	mux.HandleFunc("POST /conversations", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "conv_partial", "object": "conversation"})
	})
	mux.HandleFunc("POST /conversations/{id}/items", func(w http.ResponseWriter, r *http.Request) {
		// The first batch succeeds; every later batch fails, leaving the first
		// batch's items committed server-side.
		if itemCalls.Add(1) >= 2 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		var body struct {
			Items []map[string]any `json:"items"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		created := make([]map[string]any, 0, len(body.Items))
		for i, it := range body.Items {
			it["id"] = "item_" + strconv.Itoa(i)
			created = append(created, it)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": created})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	// Disable client-side retries so the 500 surfaces immediately and the call
	// count stays deterministic.
	s := NewConversationsSession(
		option.WithAPIKey("test"),
		option.WithBaseURL(srv.URL+"/"),
		option.WithMaxRetries(0),
	)

	// 25 items => two batches [20, 5]; the second batch fails.
	items := make([]agents.InputItem, 0, 25)
	for i := range 25 {
		items = append(items, agents.InputItemsFromText("m"+strconv.Itoa(i))...)
	}

	err := agents.NewSession(s).AppendItems(ctx, items, agents.Source{})
	if err == nil {
		t.Fatal("AddItems returned nil, want an error when a later batch fails")
	}
	msg := err.Error()
	for _, want := range []string{"partially written", "writing 20 of 25"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
}

// TestConversationsSession_AddItemsFirstBatchFailureIsPlain verifies that a
// failure on the very first batch (nothing committed yet) does not claim a
// partial-write state.
func TestConversationsSession_AddItemsFirstBatchFailureIsPlain(t *testing.T) {
	ctx := t.Context()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /conversations", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "conv_plain", "object": "conversation"})
	})
	mux.HandleFunc("POST /conversations/{id}/items", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	s := NewConversationsSession(
		option.WithAPIKey("test"),
		option.WithBaseURL(srv.URL+"/"),
		option.WithMaxRetries(0),
	)

	err := agents.NewSession(s).AppendItems(ctx, agents.InputItemsFromText("only"), agents.Source{})
	if err == nil {
		t.Fatal("AddItems returned nil, want an error")
	}
	if strings.Contains(err.Error(), "partially written") {
		t.Errorf("first-batch failure must not claim a partial-write state: %v", err)
	}
}
