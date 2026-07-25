// Command tools demonstrates a function tool with a typed argument struct.
//
// Run with: OPENAI_API_KEY=... go run ./examples/tools
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/openai"
)

type weatherArgs struct {
	City string `json:"city" jsonschema:"the city to look up weather for"`
}

func main() {
	getWeather := agents.NewFunctionTool("get_weather", "Look up the current weather for a city.",
		func(ctx context.Context, tc *agents.ToolContext, args weatherArgs) (string, error) {
			// A real tool would call a weather API here.
			return fmt.Sprintf("It is sunny and 22°C in %s.", args.City), nil
		})

	agent := &agents.Agent{
		Name:         "weather-bot",
		Instructions: agents.StaticInstructions("Answer weather questions using the get_weather tool."),
		Model:        "gpt-4o",
		Tools:        []agents.Tool{getWeather},
	}

	res, err := agents.RunSync(context.Background(), agent, "上海今天天气怎么样？", agents.RunOptions{
		ModelProvider: openai.NewProvider(),
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.FinalOutputString())
}
