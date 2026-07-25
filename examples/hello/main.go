// Command hello is the simplest agents-go example: one agent, one question.
//
// Run with: OPENAI_API_KEY=... go run ./examples/hello
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/openai"
)

func main() {
	provider := openai.NewProvider() // reads OPENAI_API_KEY

	agent := &agents.Agent{
		Name:         "assistant",
		Instructions: agents.StaticInstructions("You are a concise, helpful assistant."),
		Model:        "gpt-4o",
	}

	res, err := agents.RunSync(context.Background(), agent, "用一句话介绍 Go 语言。", agents.RunOptions{
		ModelProvider: provider,
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.FinalOutputString())
}
