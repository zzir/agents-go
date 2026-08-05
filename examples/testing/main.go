// Command testing shows how to test an agent you built, without calling a
// model. The agent lives here; its tests live in agent_test.go and run with
// no API key at all.
//
//	OPENAI_API_KEY=... go run ./examples/testing   # the real thing
//	go test ./examples/testing                     # the tests, offline
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/openai"
)

type weatherArgs struct {
	City string `json:"city" jsonschema:"the city to look up"`
}

// weatherTool is the tool under test. A real one would call a weather service;
// what matters for the test is that the agent decides to call it and does
// something sensible with the answer.
var weatherTool = agents.NewFunctionTool("get_weather", "Look up the current weather for a city.",
	func(ctx context.Context, tc *agents.ToolContext, args weatherArgs) (string, error) {
		return "sunny, 23°C in " + args.City, nil
	})

// newAgent builds the agent under test. Tests construct it the same way the
// program does, so what they exercise is what ships — the only difference is
// the model, which RunOptions supplies.
func newAgent() *agents.Agent {
	return &agents.Agent{
		Name:         "weather assistant",
		Instructions: agents.StaticInstructions("Answer weather questions. Use get_weather; do not guess."),
		Model:        "gpt-4o",
		Tools:        []*agents.FunctionTool{weatherTool},
	}
}

func main() {
	provider := openai.NewProvider() // reads OPENAI_API_KEY

	res, err := agents.RunSync(context.Background(), newAgent(), "What's the weather in SF?",
		agents.RunOptions{Model: agents.ModelOptions{Provider: provider}})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.FinalOutputString())
}
