// Command anthropic streams an agent run on Claude through the Anthropic
// Messages API provider. The provider translates the Messages SSE stream into
// the SDK's canonical response.* events at the model boundary, so streaming,
// tools and sessions work exactly as with the OpenAI provider.
//
// Run with: ANTHROPIC_API_KEY=... go run .
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/anthropic"
)

type weatherArgs struct {
	City string `json:"city" jsonschema:"the city to look up weather for"`
}

func main() {
	getWeather := agents.NewFunctionTool("get_weather", "Look up the current weather for a city.",
		func(ctx context.Context, tc *agents.ToolContext, args weatherArgs) (string, error) {
			return fmt.Sprintf("It is sunny and 22°C in %s.", args.City), nil
		})

	agent := &agents.Agent{
		Name:         "claude-weather-bot",
		Instructions: agents.StaticInstructions("Answer weather questions using the get_weather tool."),
		Model:        "claude-opus-5",
		Tools:        []*agents.FunctionTool{getWeather},
	}

	stream, _ := agents.Run(context.Background(), agent, "What's the weather in Oslo?", agents.RunOptions{
		Model: agents.ModelOptions{Provider: anthropic.NewProvider()},
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
			if tc, ok := e.Item.(*agents.ToolCallItem); ok {
				fmt.Println("tool call:", tc.FunctionCall().Name)
			}
		case *agents.RunCompletedEvent:
			res = e.Result
		}
	}

	fmt.Println("\nfinal:", res.FinalOutputString())
}
