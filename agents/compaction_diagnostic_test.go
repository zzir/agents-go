package agents_test

import (
	"context"
	"errors"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/internal/agentstest"
)

// failingCompactor gives up on every pass.
type failingCompactor struct{}

func (failingCompactor) Compact(context.Context, []session.Entry) ([]session.Entry, error) {
	return nil, errors.New("summarizer offline")
}

// A compaction pass that gives up before the first turn is trouble the run
// survived like any other: it is on the result's diagnostics.
func TestBeforeRunCompactionFailureIsADiagnostic(t *testing.T) {
	ctx := context.Background()
	sess := session.NewInMemorySession()
	agent := &agents.Agent{Name: "a", ModelImpl: agentstest.TextModel("first", "second")}
	// A first run leaves history for the second run's before-run pass to see.
	if _, err := agents.RunSync(ctx, agent, "hi", agents.RunOptions{
		Conversation: agents.ConversationOptions{Session: sess},
	}); err != nil {
		t.Fatal(err)
	}

	res, err := agents.RunSync(ctx, agent, "again", agents.RunOptions{
		Conversation: agents.ConversationOptions{Session: sess},
		Compaction:   agents.CompactionOptions{Compactor: failingCompactor{}, Points: agents.CompactBeforeRun},
	})
	if err != nil {
		t.Fatalf("the run must survive a failed compaction pass: %v", err)
	}
	for _, d := range res.Diagnostics {
		if d.Type == agents.DiagCompactionFailed {
			return
		}
	}
	t.Fatalf("no %s diagnostic on the result: %+v", agents.DiagCompactionFailed, res.Diagnostics)
}
