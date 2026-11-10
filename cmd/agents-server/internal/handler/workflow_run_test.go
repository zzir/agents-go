package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// A workflow start the hub refuses answers what every other start does — 409
// for a session mid-delete, 503 for a draining server — never a 500.
func TestWorkflowRunAnswersTheHubsRefusals(t *testing.T) {
	r := authzRig(t)
	ctx := context.Background()
	pv := &store.Provider{Name: "endpoint", APIKey: "k", BaseURL: "http://127.0.0.1:1", OwnerID: memberUser.ID}
	if err := store.NewProviderStore(r.db).Create(ctx, pv); err != nil {
		t.Fatal(err)
	}
	ac := &store.AgentConfig{Name: "worker", Model: "gpt-test", ProviderID: pv.ID, OwnerID: memberUser.ID}
	if err := store.NewAgentConfigStore(r.db).Create(ctx, ac); err != nil {
		t.Fatal(err)
	}
	wf := &store.Workflow{Name: "build", OwnerID: memberUser.ID,
		Steps: store.WorkflowSteps{{ID: store.NewID(), Name: "one", AgentConfigID: ac.ID, Prompt: "p"}}}
	if err := store.NewWorkflowStore(r.db).Create(ctx, wf); err != nil {
		t.Fatal(err)
	}
	sess := &store.Session{OwnerID: memberUser.ID, ID: store.NewID(), Name: "mine"}
	if err := r.sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	run := func() (int, string) {
		rec := serve(r.engine, as(memberUser, http.MethodPost, "/api/v1/workflows/"+wf.ID+"/runs", `{"session_id":"`+sess.ID+`","input":"go"}`))
		return rec.Code, rec.Body.String()
	}

	r.runner.StopSessionTree(sess.ID) // the delete cascade has begun
	if code, body := run(); code != http.StatusConflict || !strings.Contains(body, "being deleted") {
		t.Fatalf("run into a session mid-delete = %d %s, want 409 naming the delete", code, body)
	}
	r.runner.EndSessionDelete(sess.ID)

	drain, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	r.runner.Shutdown(drain)
	if code, body := run(); code != http.StatusServiceUnavailable {
		t.Fatalf("run into a draining server = %d %s, want 503", code, body)
	}
}
