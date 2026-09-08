package bridge

import (
	"testing"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// The agent scope is writable only when the config allows it AND the owner
// may edit the agent; then every write waits for approval. Session memory is
// always the first, writable, approval-free scope.
func TestMemoryScopesFollowTheEditRule(t *testing.T) {
	owner, other := store.NewID(), store.NewID()
	private := &BuildResult{Memory: store.MemoryGroup{Tools: true, AgentWrite: true}, ConfigScope: store.ScopePrivate, ConfigOwnerID: owner}
	global := &BuildResult{Memory: store.MemoryGroup{Tools: true, AgentWrite: true}, ConfigScope: store.ScopeGlobal, ConfigOwnerID: owner}
	off := &BuildResult{Memory: store.MemoryGroup{Tools: true}, ConfigScope: store.ScopePrivate, ConfigOwnerID: owner}
	cases := []struct {
		name     string
		built    *BuildResult
		caller   string
		admin    bool
		writable bool
	}{
		{"owner of a private agent", private, owner, false, true},
		{"another member on a private agent", private, other, false, false},
		{"admin on a private agent", private, other, true, false},
		{"owner of a global agent", global, owner, false, true},
		{"member on a global agent", global, other, false, false},
		{"admin on a global agent", global, other, true, true},
		{"agent writes off", off, owner, false, false},
	}
	for _, c := range cases {
		specs := memoryScopes(c.built, c.caller, c.admin)
		if len(specs) != 2 || specs[0].Name != "session" || !specs[0].Writable || specs[0].Approve {
			t.Fatalf("%s: session spec = %+v", c.name, specs[0])
		}
		agent := specs[1]
		if agent.Name != "agent" || agent.Writable != c.writable || agent.Approve != c.writable {
			t.Fatalf("%s: agent spec = %+v, want writable=%v", c.name, agent, c.writable)
		}
		if !c.writable && agent.Describe == "" {
			t.Fatalf("%s: a read-only scope says why", c.name)
		}
	}
}
