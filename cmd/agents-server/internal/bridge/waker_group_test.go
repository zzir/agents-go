package bridge

import (
	"testing"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

func wk(id, inherit, payload, parentRun string) store.Wakeup {
	return store.Wakeup{ID: id, Inherit: inherit, Payload: payload, ParentRunID: parentRun}
}

// A drain delivers only the oldest inherit group; different-inherit debts wait
// for their own turn, so no turn ever runs some debts under the wrong agent.
func TestOldestInheritGroup(t *testing.T) {
	agentA := string(store.EncodeInherit(store.Inherit{AgentConfigID: "A"}))
	agentB := string(store.EncodeInherit(store.Inherit{AgentConfigID: "B"}))

	pending := []store.Wakeup{
		wk("1", agentA, "a1", "runA"),
		wk("2", agentB, "b1", "runB"),
		wk("3", agentA, "a2", "runA2"),
	}
	batch, inherit, parentRun := oldestInheritGroup(pending)
	if inherit.AgentConfigID != "A" || parentRun != "runA" {
		t.Fatalf("anchor = %q/%q, want the oldest (A/runA)", inherit.AgentConfigID, parentRun)
	}
	if len(batch) != 2 || batch[0].ID != "1" || batch[1].ID != "3" {
		t.Fatalf("batch = %+v, want both agent-A debts and nothing from B", batch)
	}
}
