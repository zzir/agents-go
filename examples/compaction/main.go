// Command compaction shows openai.CompactionSession: it wraps any Session and
// calls the OpenAI responses.compact API to summarize history once it grows past
// a threshold, keeping the stored conversation small over a long chat.
package main

import (
	"context"
	"fmt"
	"log"

	agents "github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/openai"
)

func main() {
	ctx := context.Background()
	provider := openai.NewProvider() // reads OPENAI_API_KEY

	// Wrap an in-memory session; compact once 10 candidate items accumulate.
	sess, err := openai.NewCompactionSession(agents.NewInMemorySession(), openai.CompactionOptions{
		Model:     "gpt-4.1",
		Threshold: 10,
	})
	if err != nil {
		log.Fatal(err)
	}

	agent := &agents.Agent{Name: "assistant", Model: "gpt-4o"}
	opts := agents.RunOptions{Conversation: agents.ConversationOptions{Session: sess}, Model: agents.ModelOptions{Provider: provider}}

	prompts := []string{
		"My favorite color is teal.",
		"I have a dog named Pixel.",
		"I live in Lisbon.",
		"What do you remember about me?",
	}
	for _, p := range prompts {
		res, err := agents.RunSync(ctx, agent, p, opts)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("> %s\n%s\n\n", p, res.FinalOutputString())
	}

	items, _ := agents.SessionItems(ctx, sess, 0)
	fmt.Printf("stored items after the chat: %d (compaction keeps this bounded)\n", len(items))
}
