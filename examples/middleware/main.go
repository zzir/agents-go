// Command middleware stacks two run middlewares, outermost first: Loop re-runs
// the agent until an evaluator accepts the answer; Approval answers approval
// pauses from a standing rule. Approval sits inside Loop — see spec §2.12.
//
// Run with: OPENAI_API_KEY=... go run ./examples/middleware
package main

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/middleware"
	"github.com/zzir/agents-go/models/openai"
)

type lookupArgs struct {
	Topic string `json:"topic" jsonschema_description:"What to look up."`
}

func main() {
	ctx := context.Background()
	provider := openai.NewProvider() // reads OPENAI_API_KEY

	// Needs approval; the policy below answers for it, so no resume loop here.
	lookup := agents.NewTool("lookup", "Look a topic up in the archive.",
		func(_ context.Context, _ *agents.ToolContext, a lookupArgs) (string, error) {
			return "The archive says: " + a.Topic + " was first described in 1957.", nil
		})
	lookup.NeedsApproval = true

	agent := &agents.Agent{
		Name:         "researcher",
		Model:        "gpt-4.1-mini",
		Instructions: agents.StaticInstructions("Answer using the lookup tool. Keep it to one sentence."),
		Tools:        []*agents.Tool{lookup},
	}

	attempt := 0
	opts := agents.RunOptions{
		Model: agents.ModelOptions{Provider: provider},
		Middlewares: []agents.RunMiddleware{
			// Judge the answer, and say why when rejecting it.
			middleware.Loop{
				MaxAttempts: 3,
				Evaluate: func(_ context.Context, res *agents.RunResult) (middleware.Evaluation, error) {
					attempt++
					out := res.FinalOutputString()
					fmt.Printf("  [attempt %d] %s\n", attempt, out)
					if strings.Contains(out, "1957") {
						return middleware.Stop(), nil
					}
					return middleware.Continue("You must quote the year from the archive verbatim."), nil
				},
			},
			// Innermost: each attempt's pause is answered before the evaluator sees it.
			middleware.Approval{Policy: func(_ context.Context, item *agents.ToolApprovalItem) (middleware.Decision, string) {
				if item.ToolName == "lookup" {
					fmt.Printf("  [policy] approving %s(%s)\n", item.ToolName, item.Arguments)
					return middleware.Allow, ""
				}
				// Anything the rule does not cover still reaches the caller.
				return middleware.Ask, ""
			}},
		},
	}

	res, err := agents.RunSync(ctx, agent, "When was the transistor radio first described?", opts)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\naccepted after %d attempt(s):\n%s\n", attempt, res.FinalOutputString())
}
