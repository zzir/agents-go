package bridge

import (
	"testing"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

func wk(id, inherit, payload, parentRun string) store.Wakeup {
	return store.Wakeup{ID: id, Inherit: inherit, Payload: payload, ParentRunID: parentRun}
}

// A task's results and a trigger's deliveries are paid apart, the deliveries
// grouped by trigger in the order each is first owed.
func TestSplitOwed(t *testing.T) {
	fire := func(id, trigger string) store.Wakeup {
		return store.Wakeup{ID: id, Kind: store.WakeKindTrigger, SourceID: trigger}
	}
	task := store.Wakeup{ID: "2", Kind: store.WakeKindTask, SourceID: "task"}
	results, deliveries := splitOwed([]store.Wakeup{fire("1", "B"), task, fire("3", "A"), fire("4", "B")})
	if len(results) != 1 || results[0].ID != "2" {
		t.Fatalf("results = %+v, want the task's alone", results)
	}
	if len(deliveries) != 2 || len(deliveries[0]) != 2 || deliveries[0][0].ID != "1" || deliveries[0][1].ID != "4" ||
		len(deliveries[1]) != 1 || deliveries[1][0].ID != "3" {
		t.Fatalf("deliveries = %+v, want B's two, then A's one", deliveries)
	}
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
