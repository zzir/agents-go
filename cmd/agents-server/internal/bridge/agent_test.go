package bridge

import (
	"context"
	"strings"
	"testing"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// TestBuildFullAgentRejectsUsePreviousResponseID locks the run-time safety
// net for configs saved before the API rejected the flag: agents-server
// always runs with a persisted session, which the SDK refuses to combine with
// UsePreviousResponseID, so the build must fail with an error pointing at the
// config field instead of surfacing the SDK error mid-run.
func TestBuildFullAgentRejectsUsePreviousResponseID(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	agentConfigs := store.NewAgentConfigStore(db)

	ac := &store.AgentConfig{Name: "legacy", Model: "gpt-test", Session: store.SessionGroup{UsePreviousResponseID: true}}
	if err := agentConfigs.Create(ctx, ac); err != nil {
		t.Fatalf("create agent config: %v", err)
	}
	deps := &AgentDeps{
		AgentConfigs: agentConfigs,
		Settings:     store.NewSettingStore(db),
		Memories:     store.NewMemoryStore(db),
	}

	_, err := BuildFullAgent(ctx, deps, ac.ID, "")
	if err == nil || !strings.Contains(err.Error(), "use_previous_response_id") {
		t.Fatalf("want an error naming use_previous_response_id, got %v", err)
	}

	// Clearing the flag makes the same config build again.
	ac.Session.UsePreviousResponseID = false
	if err := agentConfigs.Update(ctx, ac.ID, ac); err != nil {
		t.Fatalf("update agent config: %v", err)
	}
	built, err := BuildFullAgent(ctx, deps, ac.ID, "")
	if err != nil {
		t.Fatalf("build after clearing the flag: %v", err)
	}
	if built.UsePreviousResponseID {
		t.Error("cleared flag should not survive into the build result")
	}
}
