// Command steering demonstrates RunControl's three injection queues: putting
// input into a run that is already going.
//
// They are three because they are consumed at different points, and only two of
// them may extend a run that was ending:
//
//	Steer     — the next model call, whatever the run is doing; forces another
//	            turn even if the agent was about to finish. "Change course."
//	NextTurn  — the next turn boundary, if there is one. Never extends the run.
//	FollowUp  — after the final output, continuing the SAME run. "And then."
//
// Run with: OPENAI_API_KEY=... go run ./examples/steering
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/openai"
)

func main() {
	ctx := context.Background()
	provider := openai.NewProvider() // reads OPENAI_API_KEY

	agent := &agents.Agent{
		Name:         "writer",
		Model:        "gpt-4.1-mini",
		Instructions: agents.StaticInstructions("Answer in one short sentence."),
	}

	stream, ctrl := agents.Run(ctx, agent, "Name a colour.", agents.RunOptions{
		Model: agents.ModelOptions{Provider: provider},
	})

	// Queued before the run reaches its final output, so it continues in the
	// SAME run rather than starting a new one — the trace, the usage total and
	// the session stay one thing.
	if err := ctrl.FollowUp("Now name a fruit of that colour."); err != nil {
		log.Fatal(err)
	}

	var final *agents.RunResult
	for event, err := range stream {
		if err != nil {
			log.Fatal(err)
		}
		if e, ok := event.(*agents.RunCompletedEvent); ok {
			final = e.Result
		}
	}
	if final == nil {
		log.Fatal("run produced no result")
	}

	fmt.Printf("final answer: %s\n", final.FinalOutputString())
	fmt.Printf("model calls in this ONE run: %d\n", len(final.RawResponses))

	// Anything a run never consumed is reported rather than silently dropped.
	// A NextTurn queued after the run ended is exactly that case.
	if err := ctrl.NextTurn("this arrives too late"); err != nil {
		log.Fatal(err)
	}
	if p := ctrl.Pending(); !p.Empty() {
		fmt.Printf("undelivered: %d next-turn item(s) — reported, not dropped\n", len(p.NextTurn))
	}
}
