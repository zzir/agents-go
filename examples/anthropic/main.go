// Command anthropic streams an agent run on Claude through the Anthropic
// Messages API provider, which translates the Messages SSE stream into the
// SDK's canonical response.* events — see decisions §5.10.
//
// Run with: ANTHROPIC_API_KEY=... go run .
// An older model takes the effort as a token budget: go run . -model claude-haiku-4-5
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"strings"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/anthropic"
)

type weatherArgs struct {
	City string `json:"city" jsonschema:"the city to look up weather for"`
}

func main() {
	model := flag.String("model", "claude-opus-5", "the Claude model to run")
	flag.Parse()

	getWeather := agents.NewTool("get_weather", "Look up the current weather for a city.",
		func(ctx context.Context, tc *agents.ToolContext, args weatherArgs) (string, error) {
			return fmt.Sprintf("It is sunny and 22°C in %s.", args.City), nil
		})

	agent := &agents.Agent{
		Name:         "claude-weather-bot",
		Instructions: agents.StaticInstructions("Answer weather questions using the get_weather tool."),
		Model:        *model,
		Tools:        []*agents.Tool{getWeather},
		// Sent as adaptive thinking with this effort.
		ModelSettings: &agents.ModelSettings{Reasoning: &agents.Reasoning{Effort: agents.ReasoningEffortHigh}},
	}

	// Models before adaptive thinking take the effort as a thinking budget —
	// see decisions §5.76.
	provider := anthropic.NewProvider()
	if strings.HasPrefix(*model, "claude-haiku-") {
		provider.WithBudgetThinking(true)
	}

	stream, _ := agents.Run(context.Background(), agent, "What's the weather in Oslo?", agents.RunOptions{
		Model: agents.ModelOptions{Provider: provider},
	})

	var res *agents.RunResult
	for event, err := range stream {
		if err != nil {
			log.Fatal(err)
		}
		switch e := event.(type) {
		case *agents.RawResponsesStreamEvent:
			// Token deltas, synthesized from the Messages SSE stream.
			if e.Data.Type == "response.output_text.delta" {
				fmt.Print(e.Data.AsResponseOutputTextDelta().Delta)
			}
		case *agents.RunItemStreamEvent:
			if e.Item.Kind == agents.ItemToolCall {
				tc := e.Item
				fmt.Println("tool call:", tc.FunctionCall().Name)
			}
		case *agents.RunCompletedEvent:
			res = e.Result
		}
	}

	fmt.Println("\nfinal:", res.FinalOutputString())
}
