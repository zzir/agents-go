package openai

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/openai/openai-go/v3/option"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
)

// fakeConversations is a minimal in-memory stand-in for the OpenAI Conversations
// API, enough to exercise ConversationsSession offline.
type fakeConversations struct {
	mu      sync.Mutex
	items   []map[string]any
	seq     int
	deleted bool
}

func (f *fakeConversations) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /conversations", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"id": "conv_test", "object": "conversation"})
	})

	mux.HandleFunc("POST /conversations/{id}/items", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Items []map[string]any `json:"items"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		created := []map[string]any{}
		for _, it := range body.Items {
			f.seq++
			it["id"] = "item_" + strconv.Itoa(f.seq)
			f.items = append(f.items, it)
			created = append(created, it)
		}
		f.mu.Unlock()
		writeJSON(w, map[string]any{"object": "list", "data": created})
	})

	mux.HandleFunc("GET /conversations/{id}/items", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		data := slices.Clone(f.items)
		f.mu.Unlock()
		if r.URL.Query().Get("order") == "desc" {
			slices.Reverse(data)
		}
		if l := r.URL.Query().Get("limit"); l != "" {
			if n, _ := strconv.Atoi(l); n > 0 && n < len(data) {
				data = data[:n]
			}
		}
		last := ""
		if len(data) > 0 {
			last, _ = data[len(data)-1]["id"].(string)
		}
		writeJSON(w, map[string]any{"object": "list", "data": data, "has_more": false, "last_id": last})
	})

	mux.HandleFunc("DELETE /conversations/{id}/items/{item}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("item")
		f.mu.Lock()
		f.items = slices.DeleteFunc(f.items, func(m map[string]any) bool { return m["id"] == id })
		f.mu.Unlock()
		writeJSON(w, map[string]any{"id": "conv_test", "object": "conversation"})
	})

	mux.HandleFunc("DELETE /conversations/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.items = nil
		f.deleted = true
		f.mu.Unlock()
		writeJSON(w, map[string]any{"id": r.PathValue("id"), "object": "conversation.deleted", "deleted": true})
	})

	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func newTestSession(t *testing.T) (*ConversationsSession, *fakeConversations) {
	t.Helper()
	fake := &fakeConversations{}
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	s := NewConversationsSession(option.WithAPIKey("test"), option.WithBaseURL(srv.URL+"/"))
	return s, fake
}

func TestConversationsSession_AddGetClear(t *testing.T) {
	ctx := t.Context()
	s, fake := newTestSession(t)

	// Empty AddItems is a no-op and does not create a conversation.
	if err := session.NewSession(s).AppendItems(ctx, nil, agents.Source{}); err != nil {
		t.Fatalf("empty AddItems: %v", err)
	}

	if err := session.NewSession(s).AppendItems(ctx, agents.InputItemsFromText("hello"), agents.Source{}); err != nil {
		t.Fatal(err)
	}
	if err := session.NewSession(s).AppendItems(ctx, agents.InputItemsFromText("world"), agents.Source{}); err != nil {
		t.Fatal(err)
	}

	all, err := session.NewSession(s).ContextItems(ctx, session.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("GetItems(0) len = %d, want 2", len(all))
	}

	// Limit returns the most recent item, oldest-first ordering preserved.
	recent, err := session.NewSession(s).ContextItems(ctx, session.Cursor{Limit: -1})
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 1 {
		t.Fatalf("GetItems(1) len = %d, want 1", len(recent))
	}
	if !itemContains(t, recent[0], "world") {
		t.Errorf("GetItems(1) item does not contain %q", "world")
	}

	// Clear deletes the server-side conversation.
	if err := s.Clear(ctx); err != nil {
		t.Fatal(err)
	}
	if !fake.deleted {
		t.Error("Clear did not delete the conversation")
	}
}

func itemContains(t *testing.T, item agents.InputItem, sub string) bool {
	t.Helper()
	b, err := session.MarshalInputItem(item)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Contains(string(b), sub)
}

// itemBatchRecorder fakes the Conversations endpoints AddItems touches and
// records the item count of every POST /conversations/{id}/items request.
type itemBatchRecorder struct {
	mu      sync.Mutex
	batches []int
	stored  []map[string]any
	seq     int
}

func (f *itemBatchRecorder) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /conversations", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "conv_batch", "object": "conversation"})
	})
	mux.HandleFunc("POST /conversations/{id}/items", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Items []map[string]any `json:"items"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.batches = append(f.batches, len(body.Items))
		created := []map[string]any{}
		for _, it := range body.Items {
			f.seq++
			it["id"] = "item_" + strconv.Itoa(f.seq)
			f.stored = append(f.stored, it)
			created = append(created, it)
		}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": created})
	})
	return mux
}

func TestConversationsSession_AddItemsBatchesAtAPILimit(t *testing.T) {
	ctx := t.Context()
	fake := &itemBatchRecorder{}
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	s := NewConversationsSession(option.WithAPIKey("test"), option.WithBaseURL(srv.URL+"/"))

	items := make([]agents.InputItem, 0, 45)
	for i := range 45 {
		items = append(items, agents.InputItemsFromText("msg-"+strconv.Itoa(i))...)
	}
	if err := session.NewSession(s).AppendItems(ctx, items, agents.Source{}); err != nil {
		t.Fatal(err)
	}

	fake.mu.Lock()
	batches := append([]int(nil), fake.batches...)
	stored := len(fake.stored)
	fake.mu.Unlock()

	want := []int{20, 20, 5}
	if len(batches) != len(want) {
		t.Fatalf("batches = %v, want %v", batches, want)
	}
	for i, n := range want {
		if batches[i] != n {
			t.Fatalf("batches = %v, want %v", batches, want)
		}
	}
	if stored != 45 {
		t.Errorf("stored items = %d, want 45", stored)
	}
}

func TestConversationsSession_AddItemsSingleBatchUnderLimit(t *testing.T) {
	ctx := t.Context()
	fake := &itemBatchRecorder{}
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	s := NewConversationsSession(option.WithAPIKey("test"), option.WithBaseURL(srv.URL+"/"))

	if err := session.NewSession(s).AppendItems(ctx, agents.InputItemsFromText("only"), agents.Source{}); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.batches) != 1 || fake.batches[0] != 1 {
		t.Errorf("batches = %v, want [1]", fake.batches)
	}
}
