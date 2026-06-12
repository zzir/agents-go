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

	sr := agents.RunStreamed(context.Background(), agent, "讲一个关于 Gopher 的短故事。", agents.RunOptions{
		ModelProvider: openai.NewProvider(),
	})

	for event, err := range sr.Events() {
		if err != nil {
			log.Fatal(err)
		}
		switch e := event.(type) {
		case *agents.RunItemStreamEvent:
			if msg, ok := e.Item.(*agents.MessageOutputItem); ok {
				fmt.Println("message:", msg.Text())
			}
		case *agents.AgentUpdatedStreamEvent:
			fmt.Println("-> now running:", e.NewAgent.Name)
		}
	}

	res, err := sr.FinalResult()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("\nfinal:", res.FinalOutputString())
}
