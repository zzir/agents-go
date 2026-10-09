// Command fallback composes Model decorators for resilience: each backend
// retries transient failures, and the run falls back to a second backend when
// the first is exhausted — see docs/howto/models.md.
//
// Run with: OPENAI_API_KEY=... go run ./examples/fallback
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/openai"
)

func main() {
	provider := openai.NewProvider() // reads OPENAI_API_KEY

	// A production backup is another vendor:
	// openai.NewProvider(option.WithBaseURL(...), option.WithAPIKey(...)).

	primary, err := provider.Model("gpt-4o")
	if err != nil {
		log.Fatal(err)
	}
	backup, err := provider.Model("gpt-4o-mini")
	if err != nil {
		log.Fatal(err)
	}

	// Each backend retries transient errors (429/5xx/network), honoring
	// Retry-After; an attempt silent for a minute is retried too.
	policy := agents.RetryPolicy{
		MaxAttempts:    3,
		RetryIf:        openai.RetryableError,
		RetryAfter:     openai.RetryAfter,
		AttemptTimeout: time.Minute,
	}
	// Only transient errors advance the chain; a deterministic 400 fails fast
	// — see docs/howto/models.md. A stream-only backend is adapted innermost:
	// primary = agents.NewStreamOnlyModel(primary) (decisions §5.15).
	model := agents.NewFallbackModel(
		agents.NewRetryModel(primary, policy),
		agents.NewRetryModel(backup, policy),
	).WithShouldFallback(openai.RetryableError)

	agent := &agents.Agent{
		Name:         "resilient-bot",
		Instructions: agents.StaticInstructions("You are a helpful assistant."),
		ModelImpl:    model,
	}

	res, err := agents.RunSync(context.Background(), agent, "In one sentence, what is a fallback chain?", agents.RunOptions{})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.FinalOutputString())
}
