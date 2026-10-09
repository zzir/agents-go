// Command planmode demonstrates plan mode: middleware.Plan confines the agent to
// read-only tools until a plan submitted through submit_plan is approved, then
// the SAME run continues into execution. A checklist tool of the program's own
// sits beside it — see spec §2.12 and docs/howto/running_agents.md.
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

type checklistArgs struct {
	Steps []checklistStep `json:"steps" jsonschema:"The complete checklist. It replaces the previous one entirely."`
}

type checklistStep struct {
	Content string `json:"content" jsonschema:"The step, as a short imperative phrase."`
	Status  string `json:"status" jsonschema:"pending, in_progress or completed."`
}

// updateChecklist is the agent's working list: each call replaces it whole.
func updateChecklist(_ context.Context, _ *agents.ToolContext, a checklistArgs) (string, error) {
	fmt.Println("  [checklist]")
	for i, step := range a.Steps {
		switch step.Status {
		case "pending", "in_progress", "completed":
		default:
			return "", fmt.Errorf("step %d has status %q (want pending, in_progress or completed)", i, step.Status)
		}
		fmt.Printf("    - [%s] %s\n", step.Status, step.Content)
	}
	return fmt.Sprintf("Checklist updated: %d steps.", len(a.Steps)), nil
}

func main() {
	ctx := context.Background()
	provider := openai.NewProvider() // reads OPENAI_API_KEY

	// read_file is in DefaultReadOnlyTools, so it stays usable while planning;
	// write_file is refused until the plan is approved.
	readFile := agents.NewTool("read_file", "Read the project notes.",
		func(context.Context, *agents.ToolContext, readArgs) (string, error) {
			return "NOTES: the greeting in hello.txt is outdated.", nil
		})
	writeFile := agents.NewTool("write_file", "Write a file.",
		func(_ context.Context, _ *agents.ToolContext, a writeArgs) (string, error) {
			fmt.Printf("  [write_file] %s <- %q\n", a.Path, a.Text)
			return "written", nil
		})

	checklist := agents.NewTool("update_checklist",
		"Replace your checklist of steps. Send the COMPLETE list every time; keep one step in_progress.",
		updateChecklist)

	agent := &agents.Agent{
		Name:         "worker",
		Model:        "gpt-4.1-mini",
		Instructions: agents.StaticInstructions("Fix the outdated greeting. Keep a checklist of your steps. Be brief."),
		Tools:        []*agents.Tool{readFile, writeFile, checklist},
	}

	plan := middleware.Plan{}
	opts := agents.RunOptions{
		Model:       agents.ModelOptions{Provider: provider},
		Middlewares: []agents.RunMiddleware{plan},
	}

	// The gate's own predicate, so a host can list up front what stays usable
	// while planning.
	readOnly := plan.ReadOnlySet()
	fmt.Print("usable while planning:")
	for _, t := range agent.Tools {
		if readOnly.Admits(t, false) {
			fmt.Printf(" %s", t.Name)
		}
	}
	fmt.Println()

	res, err := agents.RunSync(ctx, agent, "Update the greeting per the notes.", opts)
	if err != nil {
		log.Fatal(err)
	}

	// The plan review: an interruption on PlanToolName carries the plan in its
	// arguments. A real host shows it to a human; this approves whatever arrives.
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
