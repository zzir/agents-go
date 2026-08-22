package bridge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// inventedToolModel calls a tool the agent does not have on its first turn,
// then answers on the second — a model correcting itself once told.
func inventedToolModel(t *testing.T, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		send := sseWriter(w)
		sseCreated(send)
		var output []any
		if calls.Add(1) == 1 {
			output = []any{map[string]any{
				"type": "function_call", "id": "fc_1", "call_id": "call_1",
				"name": "exec_command", "arguments": `{"command":"ls"}`, "status": "completed",
			}}
		} else {
			output = []any{map[string]any{
				"type": "message", "id": "msg_1", "status": "completed", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": "here is the code", "annotations": []any{}}},
			}}
		}
		send("response.completed", map[string]any{
			"type": "response.completed", "sequence_number": 1,
			"response": map[string]any{
				"id": "resp_1", "object": "response", "created_at": 0, "status": "completed", "model": "gpt-test",
				"output": output,
				"usage":  map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
			},
		})
	}))
}

// A tool name the agent does not have must not take the run down. Models invent
// them, plan mode HIDES real ones, and a session with no sandbox never had the
// sandbox tools at all — so the run tells the model and carries on. Aborting
// took the turn with it, and any workflow the turn was a step of.
func TestInventedToolNameDoesNotEndTheRun(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	srv := inventedToolModel(t, &calls)
	defer srv.Close()

	runner, sessions, _, agentConfigs := newTaskTestRunner(t)
	ac := &store.AgentConfig{
		Name: "coder", Model: "gpt-test",
		ProviderID: testProvider(t, runner.db, "endpoint", "k", srv.URL),
	}
	if err := agentConfigs.Create(ctx, ac); err != nil {
		t.Fatal(err)
	}
	sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "chat"}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}

	done := make(chan *RunOutcome, 1)
	if _, err := runner.StartRun(sess.ID, ac.ID, "", "", "write a quicksort", nil, func(o *RunOutcome) {
		done <- o
	}); err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	select {
	case out := <-done:
		if out.ErrMessage != "" {
			t.Fatalf("run failed: %s", out.ErrMessage)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run never finished")
	}
	if got := calls.Load(); got < 2 {
		t.Fatalf("model called %d times, want a second turn after the tool error", got)
	}
}
