// Command steering demonstrates RunControl's three injection queues — Steer
// (the next model call), NextTurn (the next turn boundary, never extends the
// run) and FollowUp (after the final output, same run) — see spec §2.11b.
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

	// Queued before the final output, so it continues the SAME run — see spec §2.11b.
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

	// Input a run never consumed is reported, not dropped: a NextTurn after the end.
	if err := ctrl.NextTurn("this arrives too late"); err != nil {
		log.Fatal(err)
	}
	if p := ctrl.Pending(); !p.Empty() {
		fmt.Printf("undelivered: %d next-turn item(s) — reported, not dropped\n", len(p.NextTurn))
	}
}
