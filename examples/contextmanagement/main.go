// Command contextmanagement demonstrates the levers a run hands the model for
// managing its own context: the budget notice, the history tools, the memory
// tools and a new_context reset — see spec §2.5i and docs/howto/sessions.md.
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

	// One writable scope, the conversation's own notes; another scope is one
	// more ScopeSpec.
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

	// Never folds on its own; a model-requested reset carries the session memory over.
	compactor := compaction.New(&compaction.TruncationStrategy{Trigger: compaction.Never()}, nil)
	compactor.ResetSummary = func(ctx context.Context) (string, error) {
		return memory.Snapshot(ctx, notes, scopes[0].Scope, 20_000)
	}

	// Window is declared: no provider reports it. Occupied stays zero, so the
	// first call carries no figure and later calls report the last measured one.
	budget := agents.ContextBudget{Window: 128_000}.InputFilter()

	opts := agents.RunOptions{
		Model: agents.ModelOptions{
			Provider: provider,
			// Wrapped only to print the notice; a real program installs budget
			// directly.
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
