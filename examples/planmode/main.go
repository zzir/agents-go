// Command planmode demonstrates the plan/todo workflow middlewares.
//
// middleware.Plan puts the run into plan mode: the agent explores with
// read-only tools, submits a plan through submit_plan, and that call pauses
// for approval like any approval-gated tool. Approving it unlocks the rest of
// the toolset and the SAME run continues into execution; rejecting it (with a
// message) sends the model back to planning.
//
// middleware.Todo gives the agent a todo_write tool and a preamble telling it
// to keep a working list; OnUpdate is where a UI would render the checklist.
//
// The review loop below is the whole integration: an interruption whose tool
// is middleware.PlanToolName IS the plan review, and the plan text is in the
// call's arguments.
//
// Run with: OPENAI_API_KEY=... go run ./examples/planmode
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/middleware"
	"github.com/zzir/agents-go/models/openai"
)

type readArgs struct{}

type writeArgs struct {
	Path string `json:"path" jsonschema:"File to write."`
	Text string `json:"text" jsonschema:"New content."`
}

func main() {
	ctx := context.Background()
	provider := openai.NewProvider() // reads OPENAI_API_KEY

	// read_file is on middleware.DefaultReadOnlyTools, so it stays usable
	// while planning; write_file is not, so it is hidden until the plan is
	// approved.
	readFile := agents.NewFunctionTool("read_file", "Read the project notes.",
		func(context.Context, *agents.ToolContext, readArgs) (string, error) {
			return "NOTES: the greeting in hello.txt is outdated.", nil
		})
	writeFile := agents.NewFunctionTool("write_file", "Write a file.",
		func(_ context.Context, _ *agents.ToolContext, a writeArgs) (string, error) {
			fmt.Printf("  [write_file] %s <- %q\n", a.Path, a.Text)
			return "written", nil
		})

	agent := &agents.Agent{
		Name:         "worker",
		Model:        "gpt-4.1-mini",
		Instructions: agents.StaticInstructions("Fix the outdated greeting. Be brief."),
		Tools:        []agents.Tool{readFile, writeFile},
	}

	opts := agents.RunOptions{
		Model: agents.ModelOptions{Provider: provider},
		Middlewares: []agents.RunMiddleware{
			middleware.Plan{},
			middleware.Todo{OnUpdate: func(_ context.Context, items []middleware.TodoItem) {
				fmt.Println("  [todo]")
				for _, it := range items {
					fmt.Printf("    - [%s] %s\n", it.Status, it.Content)
				}
			}},
		},
	}

	res, err := agents.RunSync(ctx, agent, "Update the greeting per the notes.", opts)
	if err != nil {
		log.Fatal(err)
	}

	// The plan review loop. A real host would show the plan to a human here;
	// this example approves whatever arrives.
	for len(res.Interruptions) > 0 {
		for _, item := range res.Interruptions {
			if item.ToolName == middleware.PlanToolName {
				fmt.Printf("  [plan submitted]\n%s\n  [approving]\n", item.Arguments)
			}
			res.State.Approve(item, false)
		}
		if res, err = agents.ResumeRunSync(ctx, res.State, agents.RunOptions{
			Model: agents.ModelOptions{Provider: provider},
		}); err != nil {
			log.Fatal(err)
		}
	}

	fmt.Println("final:", res.FinalOutputString())
}
