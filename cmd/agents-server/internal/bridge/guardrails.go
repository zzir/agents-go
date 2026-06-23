package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/zzir/agents-go/agents"
)

// GuardrailDef describes an available guardrail (its name, human description,
// and whether it applies to input or output) for listing via the API.
type GuardrailDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Type        string `json:"type"`
}

var inputGuardrailDefs = []GuardrailDef{
	{Name: "content_filter", Description: "Block input containing forbidden keywords/patterns", Type: "input"},
	{Name: "max_input_length", Description: "Reject input exceeding character limit (default 50000)", Type: "input"},
}

var outputGuardrailDefs = []GuardrailDef{
	{Name: "max_output_length", Description: "Trip if output exceeds character limit (default 50000)", Type: "output"},
}

// ListGuardrails returns the catalog of available input and output guardrail definitions.
func ListGuardrails() []GuardrailDef {
	all := make([]GuardrailDef, 0, len(inputGuardrailDefs)+len(outputGuardrailDefs))
	all = append(all, inputGuardrailDefs...)
	all = append(all, outputGuardrailDefs...)
	return all
}

// BuildInputGuardrails resolves a JSON array of guardrail names into the matching input guardrails, skipping unknown names.
func BuildInputGuardrails(namesJSON string) []agents.InputGuardrail {
	var names []string
	if json.Unmarshal([]byte(namesJSON), &names) != nil || len(names) == 0 {
		return nil
	}
	var out []agents.InputGuardrail
	for _, name := range names {
		if g := lookupInputGuardrail(name); g != nil {
			out = append(out, *g)
		}
	}
	return out
}

// BuildOutputGuardrails resolves a JSON array of guardrail names into the matching output guardrails, skipping unknown names.
func BuildOutputGuardrails(namesJSON string) []agents.OutputGuardrail {
	var names []string
	if json.Unmarshal([]byte(namesJSON), &names) != nil || len(names) == 0 {
		return nil
	}
	var out []agents.OutputGuardrail
	for _, name := range names {
		if g := lookupOutputGuardrail(name); g != nil {
			out = append(out, *g)
		}
	}
	return out
}

func lookupInputGuardrail(name string) *agents.InputGuardrail {
	switch name {
	case "content_filter":
		return &agents.InputGuardrail{
			Name: "content_filter",
			Run: func(_ context.Context, _ *agents.RunContext, _ *agents.Agent, input []agents.TResponseInputItem) (agents.GuardrailFunctionOutput, error) {
				forbidden := regexp.MustCompile(`(?i)(ignore previous instructions|system prompt|jailbreak)`)
				for _, item := range input {
					raw, _ := json.Marshal(item)
					if forbidden.Match(raw) {
						return agents.GuardrailFunctionOutput{
							TripwireTriggered: true,
							OutputInfo:        "Content filter: potentially harmful input detected",
						}, nil
					}
				}
				return agents.GuardrailFunctionOutput{}, nil
			},
		}
	case "max_input_length":
		return &agents.InputGuardrail{
			Name: "max_input_length",
			Run: func(_ context.Context, _ *agents.RunContext, _ *agents.Agent, input []agents.TResponseInputItem) (agents.GuardrailFunctionOutput, error) {
				raw, _ := json.Marshal(input)
				if len(raw) > 50000 {
					return agents.GuardrailFunctionOutput{
						TripwireTriggered: true,
						OutputInfo:        fmt.Sprintf("Input too long: %d chars (max 50000)", len(raw)),
					}, nil
				}
				return agents.GuardrailFunctionOutput{}, nil
			},
		}
	default:
		return nil
	}
}

func lookupOutputGuardrail(name string) *agents.OutputGuardrail {
	switch name {
	case "max_output_length":
		return &agents.OutputGuardrail{
			Name: "max_output_length",
			Run: func(_ context.Context, _ *agents.RunContext, _ *agents.Agent, output any) (agents.GuardrailFunctionOutput, error) {
				s := fmt.Sprintf("%v", output)
				if len(s) > 50000 {
					return agents.GuardrailFunctionOutput{
						TripwireTriggered: true,
						OutputInfo:        fmt.Sprintf("Output too long: %d chars (max 50000)", len(s)),
					}, nil
				}
				return agents.GuardrailFunctionOutput{}, nil
			},
		}
	default:
		return nil
	}
}
