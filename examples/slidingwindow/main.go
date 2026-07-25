// Command slidingwindow demonstrates agents.SlidingWindowSession: a
// provider-agnostic alternative to openai.CompactionSession that summarizes
// older history with any Model you supply, keeping the newest WindowSize items
// verbatim. The split point is pair-aware — a function_call and its output
// never end up on opposite sides — and the rewrite is atomic on backends that
// implement agents.ItemsReplacer (all built-ins do).
//
// The thresholds here are tiny so a few turns trigger a compaction pass; real
// configurations keep the defaults (threshold 20 / window 10).
//
// Run with: OPENAI_API_KEY=... go run ./examples/slidingwindow
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/openai"
)

func main() {
	ctx := context.Background()
	provider := openai.NewProvider() // reads OPENAI_API_KEY

	summaryModel, err := provider.GetModel("gpt-4o-mini")
	if err != nil {
		log.Fatal(err)
	}

	// Wrap any Session; after each run the runner gives the wrapper a chance
	// to compact, and once more than Threshold items sit beyond the window it
	// replaces them with one summary message.
	sess := agents.NewSlidingWindowSession(agents.NewInMemorySession(), agents.SlidingWindowConfig{
		Threshold:    4,
		WindowSize:   2,
		SummaryModel: summaryModel,
	})

	agent := &agents.Agent{
		Name:         "assistant",
		Model:        "gpt-4o-mini",
		Instructions: agents.StaticInstructions("Answer in one short sentence."),
	}

	turns := []string{
		"My favorite city is Kyoto. Remember that.",
		"What are two dishes I should try there?",
		"Which season is best for a visit?",
		"Given everything so far, where do I want to go and when?",
	}
	for _, q := range turns {
		res, err := agents.RunSync(ctx, agent, q, agents.RunOptions{Conversation: agents.ConversationOptions{Session: sess}, Model: agents.ModelOptions{Provider: provider}})
		if err != nil {
			log.Fatal(err)
		}
		items, _ := sess.GetItems(ctx, 0)
		fmt.Printf("Q: %s\nA: %s\n(session now holds %d items)\n\n", q, res.FinalOutputString(), len(items))
	}

	// After compaction the history starts with a single summary message; the
	// final answer above still knows about Kyoto because the summary carries it.
	items, err := sess.GetItems(ctx, 0)
	if err != nil {
		log.Fatal(err)
	}
	if len(items) > 0 {
		first, _ := agents.MarshalItems(items[:1])
		fmt.Printf("first stored item after compaction:\n%s\n", first)
	}
}
