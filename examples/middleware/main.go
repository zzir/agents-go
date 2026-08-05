// Command middleware demonstrates run middleware: policy layered over the run
// loop instead of built into it.
//
// Three of them stack here, outermost first:
//
//	Retry    — re-runs the whole run if it fails outright
//	Loop     — re-runs the agent until an evaluator accepts the answer
//	Approval — answers approval pauses from a standing rule and resumes
//
// The Loop is the interesting one. The run loop knows when a model has finished
// talking and nothing more; whether the answer is good enough is the caller's
// question, and here it is answered by a rule the agent keeps forgetting.
//
// The ORDER matters, and this one is the reason: Approval must sit inside Loop.
// Outside it, Loop's first attempt would come back paused with no answer at
// all, and the evaluator would be judging an empty string.
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

	// A tool that needs approval. The policy below answers for it, so this
	// program never has to write a resume loop.
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
			// Outermost: a failure the loop could not absorb gets one more go.
			middleware.Retry{MaxAttempts: 2},

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
			// Innermost: each attempt's approval pause is answered here,
			// before the evaluator above ever sees the attempt.
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
