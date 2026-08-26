// Command guardrails shows one Guardrail value covering three stages — the run
// input, a tool's arguments and the final output — and a second one used as a
// blocking gate, which trips before any tokens are spent.
//
// Run with: OPENAI_API_KEY=... go run ./examples/guardrails
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/openai"
)

var cardNumber = regexp.MustCompile(`\b(?:\d[ -]?){13,19}\b`)

// scan renders whatever a stage handed us as JSON and looks for card numbers.
// A real scanner would be smarter; the point here is that one guardrail covers
// every stage rather than three near-copies.
func scan(v any) bool {
	b, err := json.Marshal(v)
	return err == nil && cardNumber.Match(b)
}

func main() {
	provider := openai.NewProvider()

	redact := agents.Guardrail{
		Name:   "card-numbers",
		Stages: []agents.GuardrailStage{agents.StageInput, agents.StageToolInput, agents.StageOutput},
		Run: func(ctx context.Context, rc *agents.RunContext, p agents.GuardrailPayload) (agents.GuardrailDecision, error) {
			var subject any
			switch p.Stage {
			case agents.StageInput:
				subject = p.Input
			case agents.StageToolInput:
				subject = p.Arguments
			default:
				subject = p.Output
			}
			if !scan(subject) {
				return agents.Allow(nil), nil
			}
			// Replace, not Trip: the run continues with the offending content
			// swapped out. What gets replaced depends on the stage.
			return agents.Replace("[redacted: card number]", p.Stage), nil
		},
	}

	// Blocking makes this one a gate: it runs to completion before the first
	// model call, so a tripwire costs nothing. The default is concurrent.
	refuseHomework := agents.Guardrail{
		Name:     "no-homework",
		Stages:   []agents.GuardrailStage{agents.StageInput},
		Blocking: true,
		Run: func(ctx context.Context, rc *agents.RunContext, p agents.GuardrailPayload) (agents.GuardrailDecision, error) {
			if scanFor(p.Input, "homework") {
				return agents.Trip("homework request"), nil
			}
			return agents.Allow(nil), nil
		},
	}

	lookup := agents.NewTool("lookup_account", "Look up an account by its number.",
		func(ctx context.Context, tc *agents.ToolContext, args struct {
			Number string `json:"number" jsonschema_description:"The account number."`
		}) (string, error) {
			return "account " + args.Number + ": in good standing", nil
		})

	agent := &agents.Agent{
		Name:         "support",
		Instructions: agents.StaticInstructions("You are a support agent. Use lookup_account when given a number."),
		Model:        "gpt-4o",
		Tools:        []*agents.Tool{lookup},
		Guardrails:   []agents.Guardrail{redact, refuseHomework},
	}

	opts := agents.RunOptions{Model: agents.ModelOptions{Provider: provider}}

	// 1. Redaction: the card number never reaches the model.
	res, err := agents.RunSync(context.Background(), agent,
		"Look up account 4111 1111 1111 1111 for me.", opts)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("redacted run:", res.FinalOutputString())

	// 2. Tripwire: a blocking guardrail halts the run with a typed error.
	_, err = agents.RunSync(context.Background(), agent, "Do my homework for me.", opts)
	var tripped *agents.GuardrailTripwireError
	if errors.As(err, &tripped) {
		fmt.Printf("tripped by %q before any model call\n", tripped.Result.Guardrail.Name)
		return
	}
	log.Fatalf("expected a tripwire, got %v", err)
}

// scanFor reports whether the rendered input contains needle.
func scanFor(items []agents.InputItem, needle string) bool {
	b, err := json.Marshal(items)
	return err == nil && regexp.MustCompile(`(?i)`+needle).Match(b)
}
