package bridge

import (
	"context"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// A run that fails the pre-flight config check (no API key configured for the
// agent) must still persist the user's prompt and the error. The web client
// reloads the session on run.error, so without this the optimistic message and
// the error card would be wiped to an empty session — the prompt would appear
// lost and the failure would never be shown. Mirrors the post-start error path.
func TestRunStreamed_NoAPIKeyPersistsPromptAndError(t *testing.T) {
	runner, db := newBareRunner(t)
	ctx := context.Background()

	// A valid agent config with a model but no API key: BuildFullAgent succeeds
	// yet leaves Provider nil (the Settings store carries no openai_api_key
	// fallback either), so runStreamed hits the "no API key" branch.
	cfgID := mkAgent(t, store.NewAgentConfigStore(db), "keyless")

	sess := &store.Session{ID: store.NewID(), Name: "t", AgentConfigID: cfgID}
	if err := runner.Deps.Sessions.Create(ctx, sess); err != nil {
		t.Fatalf("create session: %v", err)
	}

	res := runner.runStreamed(ctx, store.NewID(), sess.ID, cfgID, "", "hello")
	if res == nil {
		t.Fatal("runStreamed returned nil result")
	}

	// The prompt survives as a replayable user item...
	if got := countDisplays(t, db, sess.ID, agents.EntryKindItem, ""); got != 1 {
		t.Errorf("user item entries = %d, want 1 (prompt must survive the config failure)", got)
	}
	// ...alongside an error annotation, so the client's reload renders the failed
	// turn instead of an empty session.
	if got := countDisplays(t, db, sess.ID, agents.EntryKindAnnotation, agents.DisplayError); got != 1 {
		t.Errorf("error annotation entries = %d, want 1", got)
	}
}
