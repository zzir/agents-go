// Command fallback demonstrates composing Model decorators for resilience:
// each backend retries transient failures, and the run falls back to a second
// backend if the first is exhausted.
//
// Here both models come from one OpenAI provider for a single-key demo. In
// production the backup is typically a *different* provider (a second
// openai.NewProvider with option.WithBaseURL pointing at another OpenAI-compatible
// service such as Groq, Together, or a local vLLM), so an outage at one vendor
// fails over to another.
//
// Run with: OPENAI_API_KEY=... go run ./examples/fallback
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

	// A second provider would point at another vendor, e.g.:
	//   backupProvider := openai.NewProvider(
	//       option.WithBaseURL("https://api.groq.com/openai/v1"),
	//       option.WithAPIKey(os.Getenv("GROQ_API_KEY")))

	primary, err := provider.GetModel("gpt-4o")
	if err != nil {
		log.Fatal(err)
	}
	backup, err := provider.GetModel("gpt-4o-mini")
	if err != nil {
		log.Fatal(err)
	}

	// Retry transient errors (429/5xx/network) on each backend, honoring any
	// Retry-After header, then fall back from primary to backup.
	policy := agents.RetryPolicy{
		MaxAttempts: 3,
		RetryIf:     openai.RetryableError,
		RetryAfter:  openai.RetryAfter,
	}
	// By default every error except context cancellation advances the chain.
	// WithShouldFallback narrows that: with openai.RetryableError only
	// transient failures (429/5xx/network) try the backup — a deterministic
	// 400 (bad schema, context too long) fails fast instead of burning a
	// doomed call on every backend.
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
