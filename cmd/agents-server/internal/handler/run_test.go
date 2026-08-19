package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// StartRun's failure modes must map to distinct status codes rather than
// all collapsing to 404 "session not found". A busy session or a task-limit hit
// is a 409, an unknown session is a 404, and any other DB error is a 500.
func TestStartErrorMapping(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &RunHandler{}

	cases := []struct {
		name string
		err  error
		want int
	}{
		{"session busy", bridge.ErrSessionBusy{RunID: "r1"}, http.StatusConflict},
		{"task limit", bridge.ErrTaskLimit{Limit: 6}, http.StatusConflict},
		{"unknown session", fmt.Errorf("getting session s1: %w", store.ErrNotFound), http.StatusNotFound},
		{"transient db error", errors.New("database is locked"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
		h.startError(c, tc.err)
		if w.Code != tc.want {
			t.Errorf("%s: startError -> %d, want %d", tc.name, w.Code, tc.want)
		}
	}
}

// Prefer: wait=N is read per RFC 7240 — case-insensitively, among other
// preferences, across repeated headers — and only a positive N asks to wait.
func TestPreferWaitParsesTheHeader(t *testing.T) {
	cases := []struct {
		hdr  []string
		want time.Duration
		ok   bool
	}{
		{[]string{"wait=10"}, 10 * time.Second, true},
		{[]string{"respond-async, WAIT = 3"}, 3 * time.Second, true},
		{[]string{"respond-async", "wait=7"}, 7 * time.Second, true},
		{[]string{"wait=0"}, 0, false},
		{[]string{"wait=soon"}, 0, false},
		// Capped: a huge or overflowing value holds the connection no longer
		// than the ceiling.
		{[]string{"wait=99999999"}, MaxPreferWait, true},
		{[]string{"wait=9223372036854775807"}, MaxPreferWait, true},
		{[]string{"respond-async"}, 0, false},
		{nil, 0, false},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		for _, h := range c.hdr {
			r.Header.Add("Prefer", h)
		}
		got, ok := preferWait(r)
		if got != c.want || ok != c.ok {
			t.Errorf("Prefer %v: = %v,%v want %v,%v", c.hdr, got, ok, c.want, c.ok)
		}
	}
}

// slowModel answers every request with a finished message after the delay —
// long enough for a short wait to run out, short enough for the test.
func slowModel(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		resp := map[string]any{
			"id": "resp_1", "object": "response", "created_at": 0, "status": "completed", "model": "gpt-test",
			"output": []any{map[string]any{
				"type": "message", "id": "msg_1", "status": "completed", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": "hi", "annotations": []any{}}},
			}},
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
		}
		for _, ev := range []map[string]any{
			{"type": "response.created", "sequence_number": 0, "response": map[string]any{"id": "resp_1", "object": "response", "created_at": 0, "status": "in_progress", "model": "gpt-test", "output": []any{}}},
			{"type": "response.completed", "sequence_number": 1, "response": resp},
		} {
			b, _ := json.Marshal(ev)
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev["type"], b)
		}
	}))
}

// A bounded wait that runs out answers 202 with the run id — the run keeps
// going — and one that ends in time answers with the result; both mark the
// honored preference.
func TestCreateRunHonorsPreferWait(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestDB(t)
	sessions := store.NewSessionStore(db)
	sess := &store.Session{ID: store.NewID(), Name: "s"}
	if err := sessions.Create(t.Context(), sess); err != nil {
		t.Fatal(err)
	}
	srv := slowModel(t, 1500*time.Millisecond)
	t.Cleanup(srv.Close)
	pv := &store.Provider{Name: "endpoint", APIKey: "k", BaseURL: srv.URL}
	if err := store.NewProviderStore(db).Create(t.Context(), pv); err != nil {
		t.Fatal(err)
	}
	agents := store.NewAgentConfigStore(db)
	ac := &store.AgentConfig{Name: "a", Model: "gpt-test", ProviderID: pv.ID}
	if err := agents.Create(t.Context(), ac); err != nil {
		t.Fatal(err)
	}
	runner := bridge.NewRunner(t.Context(), db, &bridge.AgentDeps{
		AgentConfigs: agents, Providers: store.NewProviderStore(db), Sessions: sessions,
		Traces: store.NewTraceStore(db), Settings: settings.NewReader(store.NewSettingStore(db)), Memories: store.NewMemoryStore(db),
	})
	h := NewRunHandler(runner)
	engine := gin.New()
	engine.POST("/sessions/:id/runs", h.Create)
	body := `{"input":"hello","agent_config_id":"` + ac.ID + `"}`

	// The wait runs out first: 202, still running.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+sess.ID+"/runs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Prefer", "wait=1")
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("short wait: %d %s, want 202", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Preference-Applied"); got != "wait=1" {
		t.Fatalf("Preference-Applied = %q, want wait=1", got)
	}
	var accepted createRunResp
	if err := json.Unmarshal(w.Body.Bytes(), &accepted); err != nil || accepted.RunID == "" || accepted.Status != string(bridge.RunRunning) {
		t.Fatalf("202 body = %s (%v), want the running run", w.Body.String(), err)
	}
	// It kept running: the same session is busy, so a second start is refused —
	// and, given time, the run finishes on its own.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if info, ok := runner.Hub().Info(accepted.RunID); ok && info.Status != bridge.RunRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the run never finished after the wait ran out")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Long enough: 200 with the final output.
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/sessions/"+sess.ID+"/runs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Prefer", "wait=10")
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"final_output":"hi"`) {
		t.Fatalf("long wait: %d %s, want 200 with the output", w.Code, w.Body.String())
	}
}
