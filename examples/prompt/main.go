// Command prompt shows an agent driven by an OpenAI stored prompt instead of
// inline instructions. Create a prompt in the OpenAI dashboard, then set its ID
// (and optional version/variables) via Agent.Prompt.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	agents "github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/openai"
)

func main() {
	promptID := os.Getenv("OPENAI_PROMPT_ID")
	if promptID == "" {
		log.Fatal("set OPENAI_PROMPT_ID to a stored prompt's id (e.g. pmpt_...)")
	}

	agent := &agents.Agent{
		Name:  "assistant",
		Model: "gpt-4o",
		// No inline Instructions: the system prompt comes from the stored prompt.
		Prompt: agents.StaticPrompt(agents.Prompt{
			ID:        promptID,
			Variables: map[string]any{"tone": "concise"},
		}),
	}

	res, err := agents.RunSync(context.Background(), agent, "Say hello.",
		agents.RunOptions{Model: agents.ModelOptions{Provider: openai.NewProvider()}})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.FinalOutputString())
}
