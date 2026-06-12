// Command handoffs demonstrates a triage agent delegating to specialists.
//
// Run with: OPENAI_API_KEY=... go run ./examples/handoffs
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/openai"
)

func main() {
	billing := &agents.Agent{
		Name:               "billing",
		HandoffDescription: "Handles billing, refunds and invoices.",
		Instructions:       agents.StaticInstructions("You are a billing specialist. Be precise about charges."),
		Model:              "gpt-4o",
	}
	support := &agents.Agent{
		Name:               "support",
		HandoffDescription: "Handles technical support and troubleshooting.",
		Instructions:       agents.StaticInstructions("You are a technical support specialist."),
		Model:              "gpt-4o",
	}

	triage := &agents.Agent{
		Name:         "triage",
		Instructions: agents.StaticInstructions("Route the user to the right specialist via a handoff."),
		Model:        "gpt-4o",
		Handoffs:     []agents.Handoff{agents.HandoffTo(billing), agents.HandoffTo(support)},
	}

	res, err := agents.Run(context.Background(), triage, "我想申请上个月订单的退款。", agents.RunOptions{
		ModelProvider: openai.NewProvider(),
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("handled by %s:\n%s\n", res.LastAgent.Name, res.FinalOutputString())
}
