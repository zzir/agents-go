// Command bravesearch demonstrates giving an agent live web search via the
// Brave Search API.
//
// Run with: OPENAI_API_KEY=... BRAVE_API_KEY=... go run ./examples/bravesearch
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/openai"
	"github.com/zzir/agents-go/tools/bravesearch"
)

func main() {
	search, err := bravesearch.New(bravesearch.Options{
		// APIKey defaults to the BRAVE_API_KEY environment variable.
		Count: 5,
	})
	if err != nil {
		log.Fatal(err)
	}

	agent := &agents.Agent{
		Name:         "research-bot",
		Instructions: agents.StaticInstructions("Answer questions using the brave_search tool, and cite the URLs you used."),
		Model:        "gpt-4o",
		Tools:        []agents.Tool{search},
	}

	res, err := agents.RunSync(context.Background(), agent, "What is the latest stable Go version?", agents.RunOptions{
		ModelProvider: openai.NewProvider(),
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.FinalOutputString())
}
