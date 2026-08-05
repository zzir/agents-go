// Command streaming prints run events as they arrive.
//
// Run with: OPENAI_API_KEY=... go run ./examples/streaming
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/openai"
)

func main() {
	agent := &agents.Agent{
		Name:         "storyteller",
		Instructions: agents.StaticInstructions("Tell a very short story."),
		Model:        "gpt-4o",
	}

	// Run returns the run as a stream: nothing happens until it is ranged, and
	// the run advances on this goroutine, one event at a time.
	stream, _ := agents.Run(context.Background(), agent, "讲一个关于 Gopher 的短故事。", agents.RunOptions{Model: agents.ModelOptions{Provider: openai.NewProvider()}})

	var res *agents.RunResult
	for event, err := range stream {
		if err != nil {
			log.Fatal(err)
		}
		switch e := event.(type) {
		case *agents.RunItemStreamEvent:
			if e.Item.Kind == agents.ItemMessage {
				fmt.Println("message:", e.Item.Text())
			}
		case *agents.AgentUpdatedStreamEvent:
			fmt.Println("-> now running:", e.NewAgent.Name)
		case *agents.RunCompletedEvent:
			// The stream's last event carries the finished run.
			res = e.Result
		}
	}

	fmt.Println("\nfinal:", res.FinalOutputString())
}
