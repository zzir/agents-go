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

// A resume withdrawn after a concurrent stop already ended the reopened record
// leaves that ending in place: forcing it back to interrupted would resurrect
// a run every subscriber saw cancelled.
func TestAbortResumeKeepsATerminalStatus(t *testing.T) {
	h := NewRunHub(context.Background())
	seg, _, err := h.register("run1", "sess1", "", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	h.finish("run1", true)
	seg.finalize()

	seg2, _, reopened, err := h.resume("run1", "sess1", "", "", "", nil)
	if err != nil || !reopened {
		t.Fatalf("resume: reopened=%v err=%v", reopened, err)
	}
	// The stop lands between the reopen and verify's refusal.
	env, _ := protocol.NewEnvelope(protocol.EventRunCancelled, protocol.RunCancelled{RunID: "run1"})
	h.publish("run1", env)
	h.abortResume("run1", seg2, true)

	info, _ := h.Info("run1")
	if info.Status != RunCancelled {
		t.Fatalf("status after abortResume = %q, want the stop's cancelled kept", info.Status)
	}
	if _, busy := h.ActiveRunForSession("sess1"); busy {
		t.Fatal("abortResume must free the session slot")
	}
	// Without a stop, the withdrawal goes back to interrupted as before.
	seg3, _, _, err := h.resume("run2", "sess2", "", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	seg3.finalize()
	h.finish("run2", true)
	seg4, _, reopened, err := h.resume("run2", "sess2", "", "", "", nil)
	if err != nil || !reopened {
		t.Fatalf("resume run2: reopened=%v err=%v", reopened, err)
	}
	h.abortResume("run2", seg4, true)
	if info, _ := h.Info("run2"); info.Status != RunInterrupted {
		t.Fatalf("status after a plain abortResume = %q, want interrupted", info.Status)
	}
}
