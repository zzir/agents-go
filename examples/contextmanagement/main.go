// Command contextmanagement demonstrates the levers a run can hand the model
// for managing its own context. The first is the budget notice: every model
// call ends with one line saying how full the window is, appended to the
// input rather than the instructions so a cached prefix stays cached, and
// never written to the session.
//
// Run with: OPENAI_API_KEY=... go run ./examples/contextmanagement
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/models/openai"
)

func main() {
	ctx := context.Background()
	provider := openai.NewProvider() // reads OPENAI_API_KEY

	agent := &agents.Agent{
		Name:  "assistant",
		Model: "gpt-4.1-mini",
		Instructions: agents.StaticInstructions(
			"Answer in one sentence. Each request ends with a context budget line; mention how much is left."),
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
		Conversation: agents.ConversationOptions{Session: session.NewInMemorySession()},
	}

	for _, q := range []string{"What is the capital of Peru?", "And of Chile?"} {
		fmt.Println("user:", q)
		res, err := agents.RunSync(ctx, agent, q, opts)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println("assistant:", res.FinalOutputString())
	}
}
