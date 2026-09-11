package bridge

import (
	"context"
	"testing"

	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
)

// A reopened record carries the identity the resume was made with: a session
// transferred while its run was paused resumes under the NEW owner, so the
// attach and ownership checks keyed off the record follow the transfer.
func TestResumeReopenSyncsIdentity(t *testing.T) {
	h := NewRunHub(context.Background())
	seg, _, err := h.register("run1", "sess1", "alice", "agent1", "proj1", nil)
	if err != nil {
		t.Fatal(err)
	}
	env, _ := protocol.NewEnvelope(protocol.EventRunInterrupted, protocol.RunInterrupted{RunID: "run1"})
	h.publish("run1", env)
	h.finish("run1", true)
	seg.finalize()

	seg2, _, reopened, err := h.resume("run1", "sess1", "bob", "agent2", "proj2", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer seg2.finalize()
	if !reopened {
		t.Fatal("expected the record to be reopened")
	}
	info, _ := h.Info("run1")
	if info.OwnerID != "bob" || info.AgentConfigID != "agent2" || info.ProjectID != "proj2" {
		t.Errorf("reopened record = owner %q agent %q project %q, want bob/agent2/proj2", info.OwnerID, info.AgentConfigID, info.ProjectID)
	}
}
