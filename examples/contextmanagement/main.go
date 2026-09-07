// Command contextmanagement demonstrates the levers a run can hand the model
// for managing its own context.
//
// The budget notice: every model call ends with one line saying how full the
// window is, appended to the input rather than the instructions so a cached
// prefix stays cached, and never written to the session.
//
// The history tools: history_search and history_read read the session's log,
// folded history included, so the model can find what its context no longer
// holds. Here the third question can only be answered by searching.
//
// The memory tools: memory_write and friends give the model a memory of its
// own, by scope. The session scope is its working notes; a host may bind
// more, writable or not, behind approval or not.
//
// The reset: new_context lets the model start a fresh window at the turn's
// end; the compactor folds everything but the latest question and carries
// the session memory in its place, which is what the last question tests.
//
// Run with: OPENAI_API_KEY=... go run ./examples/contextmanagement
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/compaction"
	"github.com/zzir/agents-go/agents/history"
	"github.com/zzir/agents-go/agents/memory"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/models/openai"
)

func main() {
	ctx := context.Background()
	provider := openai.NewProvider() // reads OPENAI_API_KEY
	sess := session.NewInMemorySession()

	// One writable scope: the conversation's own notes. A second, read-only
	// or approval-gated scope would be one more ScopeSpec.
	notes := memory.NewInMemoryStore()
	scopes := []memory.ScopeSpec{{
		Scope: memory.Scope{Kind: "session", ID: "demo"}, Name: "session", Writable: true,
		Describe: "this conversation's working notes.",
	}}

	agent := &agents.Agent{
		Name:  "assistant",
		Model: "gpt-4.1-mini",
		Instructions: agents.StaticInstructions(
			"Answer in one sentence. Each request ends with a context budget line; mention how much is left. " +
				"Use history_search when asked about something said earlier. " +
				"After each answer, memory_append the capital you named to the session memory key capitals.md."),
		Tools: append(append(history.Tools(history.For(sess), history.Options{}), memory.Tools(scopes, memory.Static(notes))...),
			agents.NewContextTool()),
	}

	// A compactor that never folds on its own; a reset is the model's call,
	// and what it carries over is the session memory.
	compactor := compaction.New(&compaction.TruncationStrategy{Trigger: compaction.Never()}, nil)
	compactor.ResetSummary = func(ctx context.Context) (string, error) {
		return memory.Snapshot(ctx, notes, scopes[0].Scope, 20_000)
	}

	// The window is declared: no provider reports it. Occupied stays zero
	// here, so the very first call carries no figure and every later call
	// reports the run's own last measured call.
	budget := agents.ContextBudget{Window: 128_000}.InputFilter()

	opts := agents.RunOptions{
		Model: agents.ModelOptions{
			Provider: provider,
			// Wrapping the filter shows what the model is sent; a real program
			// installs budget.InputFilter() directly.
			InputFilter: func(ctx context.Context, rc *agents.RunContext, a *agents.Agent, data agents.ModelInputData) (agents.ModelInputData, error) {
				out, err := budget(ctx, rc, a, data)
				if err == nil && len(out.Input) > len(data.Input) {
					fmt.Println("  notice:", session.ItemText(out.Input[len(out.Input)-1]))
				}
				return out, err
			},
		},
		Conversation: agents.ConversationOptions{Session: sess},
		Compaction:   agents.CompactionOptions{Compactor: compactor},
	}

	for _, q := range []string{
		"What is the capital of Peru?",
		"And of Chile?",
		"Which capital did I ask about first?",
		"Call new_context now, then tell me which capitals you have named so far.",
	} {
		fmt.Println("user:", q)
		res, err := agents.RunSync(ctx, agent, q, opts)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println("assistant:", res.FinalOutputString())
	}

	snapshot, err := memory.Snapshot(ctx, notes, scopes[0].Scope, 0)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("session memory:\n" + snapshot)
}
