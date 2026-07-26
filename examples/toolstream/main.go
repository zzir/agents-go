// Command toolstream demonstrates streaming partial results from a tool.
//
// A tool that runs for a while leaves a consumer with nothing to show but a
// spinner. ToolContext.Emit pushes progress as the work happens, delivered as
// *agents.ToolProgressEvent on the run's stream.
//
// The rule the example makes visible: progress is NOT the answer. The model
// only ever sees what the tool RETURNS — the partials are for whoever is
// watching.
//
// Run with: OPENAI_API_KEY=... go run ./examples/toolstream
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/openai"
)

type buildArgs struct {
	Target string `json:"target" jsonschema_description:"What to build."`
}

func main() {
	ctx := context.Background()
	provider := openai.NewProvider() // reads OPENAI_API_KEY

	build := agents.NewFunctionTool("build", "Build a target and report the outcome.",
		func(_ context.Context, tc *agents.ToolContext, a buildArgs) (string, error) {
			steps := []string{"resolving dependencies", "compiling", "linking"}
			for _, step := range steps {
				// Safe from any goroutine, and a no-op on a blocking run — a
				// tool never has to ask which kind of run it is in.
				tc.Emit(agents.TextResult(step + "…\n").WithDisplay("terminal"))
				time.Sleep(150 * time.Millisecond)
			}
			// THIS is what the model sees.
			return "build succeeded: 0 errors, 2 warnings", nil
		})

	agent := &agents.Agent{
		Name:         "builder",
		Model:        "gpt-4.1-mini",
		Instructions: agents.StaticInstructions("Build what the user asks for, then report the result in one sentence."),
		Tools:        []agents.Tool{build},
	}

	stream, _ := agents.Run(ctx, agent, "Build the release target.", agents.RunOptions{
		Model: agents.ModelOptions{Provider: provider},
	})

	var final *agents.RunResult
	for event, err := range stream {
		if err != nil {
			log.Fatal(err)
		}
		switch e := event.(type) {
		case *agents.ToolProgressEvent:
			// Keyed by call id: several tools stream at once, and keying on the
			// tool name would interleave two calls to the same tool.
			fmt.Printf("  [%s %s] %s", e.ToolName, e.CallID[:6], e.Result.Text())
		case *agents.RunCompletedEvent:
			final = e.Result
		}
	}
	if final == nil {
		log.Fatal("run produced no result")
	}

	fmt.Printf("\nagent: %s\n", final.FinalOutputString())
	for _, it := range final.NewItems {
		if out, ok := it.(*agents.ToolCallOutputItem); ok {
			fmt.Printf("what the model saw: %v\n", out.Output)
		}
	}
}
