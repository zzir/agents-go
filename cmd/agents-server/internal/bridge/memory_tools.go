package bridge

import (
	"context"
	"errors"
	"fmt"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/memory"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// memoryTools builds the memory_* tools for a chat run: session memory
// always writable, agent memory under the agent's edit rule and behind
// approval (store.MemoryPolicies).
func (r *Runner) memoryTools(ctx context.Context, ownerID string, built *BuildResult) []*agents.Tool {
	admin := ownerIsAdmin(ctx, r.Deps, ownerID)
	specs := memoryScopes(built, ownerID, admin)
	rm := store.NewRunMemory(r.db, r.Deps.Memories, ownerID, admin)
	resolve := func(_ context.Context, rc *agents.RunContext, spec memory.ScopeSpec) (memory.Store, memory.Scope, error) {
		switch spec.Name {
		case store.MemoryScopeSession:
			sid, _ := rc.Context.(string)
			if sid == "" {
				return nil, memory.Scope{}, errors.New("memory: no session in the run context")
			}
			return rm, memory.Scope{Kind: store.MemoryScopeSession, ID: sid}, nil
		case store.MemoryScopeAgent:
			return rm, memory.Scope{Kind: store.MemoryScopeAgent, ID: built.ConfigID}, nil
		}
		return nil, memory.Scope{}, fmt.Errorf("memory: no scope %q", spec.Name)
	}
	return memory.Tools(specs, resolve)
}

// memoryScopes is what the model may reach: the session first, then the
// agent, writable only when the config allows it and the owner may edit the
// agent (decisions §5.29), and then only after approval.
func memoryScopes(built *BuildResult, ownerID string, admin bool) []memory.ScopeSpec {
	session := store.MemoryPolicies[store.MemoryScopeSession]
	agentP := store.MemoryPolicies[store.MemoryScopeAgent]
	specs := []memory.ScopeSpec{{
		Name: store.MemoryScopeSession, Writable: true,
		MaxBytes: session.MaxBytes, MaxKeys: session.MaxKeys,
		Describe: "this conversation's working notes, which survive compaction and a context reset.",
	}}
	agent := memory.ScopeSpec{
		Name: store.MemoryScopeAgent, MaxBytes: agentP.MaxBytes, MaxKeys: agentP.MaxKeys,
		Describe: "what every future conversation with this agent should know; short, durable facts.",
	}
	editable := ownerID == built.ConfigOwnerID || (built.ConfigScope == store.ScopeGlobal && admin)
	switch {
	case built.Memory.AgentWrite && editable:
		agent.Writable, agent.Approve = true, true
	case built.Memory.AgentWrite:
		agent.Describe += " Read-only here: the agent belongs to somebody else."
	default:
		agent.Describe += " Read-only here: the agent's settings do not let you write it."
	}
	return append(specs, agent)
}
