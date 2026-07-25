// Command errorhandlers demonstrates RunOptions.ErrorHandlers: recovering a
// run that would otherwise fail — here a tool loop that exhausts its turn
// budget, and a structured-output agent whose final message fails validation —
// by supplying a fallback final output (the counterpart of Python's
// Runner.run(..., error_handlers={...})).
//
// Run with: OPENAI_API_KEY=... go run ./examples/errorhandlers
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/openai"
)

// Report is the structured output the second agent must produce.
type Report struct {
	Summary  string `json:"summary"`
	Fallback bool   `json:"fallback" jsonschema:"true when this report was synthesized by an error handler"`
}

func main() {
	provider := openai.NewProvider() // reads OPENAI_API_KEY
	ctx := context.Background()

	// --- max_turns: a research loop that cannot finish in one turn. ---
	search := agents.NewFunctionTool("search", "Search the web for a topic.",
		func(ctx context.Context, tc *agents.ToolContext, args struct {
			Query string `json:"query"`
		}) (string, error) {
			return "No conclusive results for " + args.Query + "; search again with a narrower query.", nil
		})

	researcher := &agents.Agent{
		Name:         "researcher",
		Instructions: agents.StaticInstructions("Research the topic thoroughly. Keep searching until you are certain."),
		Model:        "gpt-4o",
		Tools:        []agents.Tool{search},
	}

	res, err := agents.RunSync(ctx, researcher, "What is the airspeed velocity of an unladen swallow?", agents.RunOptions{Exec: agents.ExecOptions{MaxTurns: 2, ErrorHandlers: // deliberately too small
	agents.RunErrorHandlers{
		MaxTurns: func(ctx context.Context, in agents.RunErrorHandlerInput) (*agents.RunErrorHandlerResult, error) {
			log.Printf("max-turns handler fired after %d responses: %v", len(in.RunData.RawResponses), in.Error)
			return &agents.RunErrorHandlerResult{
				FinalOutput: "I ran out of research budget before reaching a confident answer — try a narrower question.",
			}, nil
		},
	}}, Model: agents.ModelOptions{Provider: provider},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("researcher:", res.FinalOutputString())

	// --- invalid_final_output: a structured agent with a fallback report. ---
	reporter := &agents.Agent{
		Name:         "reporter",
		Instructions: agents.StaticInstructions("Summarize the user's text as a report."),
		Model:        "gpt-4o",
		OutputType:   agents.OutputType[Report](),
	}

	res, err = agents.RunSync(ctx, reporter, "Summarize: the quarterly numbers are up.", agents.RunOptions{Exec: agents.ExecOptions{ErrorHandlers: agents.RunErrorHandlers{
		// Fires only if the model's final message fails Report validation
		// (or it produced no final text at all).
		InvalidFinalOutput: func(ctx context.Context, in agents.RunErrorHandlerInput) (*agents.RunErrorHandlerResult, error) {
			log.Printf("invalid-final-output handler fired: %v", in.Error)
			return &agents.RunErrorHandlerResult{
				FinalOutput: Report{Summary: "Report unavailable: the model output could not be parsed.", Fallback: true},
			}, nil
		},
	}}, Model: agents.ModelOptions{Provider: provider},
	})
	if err != nil {
		log.Fatal(err)
	}
	report, _ := agents.FinalOutputAs[Report](res)
	fmt.Printf("reporter: %+v\n", report)
}
