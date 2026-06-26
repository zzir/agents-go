package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// GuardrailResolver builds SDK guardrails from stored definitions and built-in
// defaults. Names in the agent config that match a stored guardrail's name are
// resolved from the database; unrecognized names fall back to the hardcoded
// built-in set for backward compatibility.
type GuardrailResolver struct {
	store *store.GuardrailStore
}

// NewGuardrailResolver returns a resolver backed by the given store.
func NewGuardrailResolver(s *store.GuardrailStore) *GuardrailResolver {
	return &GuardrailResolver{store: s}
}

// BuildInputGuardrails resolves a JSON array of guardrail names into input guardrails.
func (r *GuardrailResolver) BuildInputGuardrails(ctx context.Context, namesJSON string) []agents.InputGuardrail {
	var names []string
	if json.Unmarshal([]byte(namesJSON), &names) != nil || len(names) == 0 {
		return nil
	}
	var out []agents.InputGuardrail
	for _, name := range names {
		if g := r.resolveInput(ctx, name); g != nil {
			out = append(out, *g)
		}
	}
	return out
}

// BuildOutputGuardrails resolves a JSON array of guardrail names into output guardrails.
func (r *GuardrailResolver) BuildOutputGuardrails(ctx context.Context, namesJSON string) []agents.OutputGuardrail {
	var names []string
	if json.Unmarshal([]byte(namesJSON), &names) != nil || len(names) == 0 {
		return nil
	}
	var out []agents.OutputGuardrail
	for _, name := range names {
		if g := r.resolveOutput(ctx, name); g != nil {
			out = append(out, *g)
		}
	}
	return out
}

// ListGuardrails returns the combined catalog of stored and built-in guardrails.
func (r *GuardrailResolver) ListGuardrails(ctx context.Context) []GuardrailDef {
	var all []GuardrailDef
	if r.store != nil {
		stored, _ := r.store.List(ctx)
		for _, g := range stored {
			all = append(all, GuardrailDef{
				ID:          g.ID,
				Name:        g.Name,
				Description: g.Description,
				Type:        g.Type,
				Mode:        g.Mode,
			})
		}
	}
	all = append(all, builtinDefs...)
	return all
}

func (r *GuardrailResolver) resolveInput(ctx context.Context, name string) *agents.InputGuardrail {
	if r.store != nil {
		if g := r.findByName(ctx, name, "input"); g != nil {
			return buildInputFromDef(g)
		}
	}
	return builtinInput(name)
}

func (r *GuardrailResolver) resolveOutput(ctx context.Context, name string) *agents.OutputGuardrail {
	if r.store != nil {
		if g := r.findByName(ctx, name, "output"); g != nil {
			return buildOutputFromDef(g)
		}
	}
	return builtinOutput(name)
}

func (r *GuardrailResolver) findByName(ctx context.Context, name, typ string) *store.Guardrail {
	all, err := r.store.List(ctx)
	if err != nil {
		return nil
	}
	for i := range all {
		if all[i].Name == name && all[i].Type == typ {
			return &all[i]
		}
	}
	return nil
}

func buildInputFromDef(g *store.Guardrail) *agents.InputGuardrail {
	var cfg store.GuardrailConfig
	_ = json.Unmarshal(g.Config, &cfg)

	switch g.Mode {
	case "regex":
		if cfg.Pattern == "" {
			return nil
		}
		re, err := regexp.Compile(cfg.Pattern)
		if err != nil {
			return nil
		}
		return &agents.InputGuardrail{
			Name: g.Name,
			Run: func(_ context.Context, _ *agents.RunContext, _ *agents.Agent, input []agents.TResponseInputItem) (agents.GuardrailFunctionOutput, error) {
				for _, item := range input {
					raw, _ := json.Marshal(item)
					if re.Match(raw) {
						return agents.GuardrailFunctionOutput{
							TripwireTriggered: true,
							OutputInfo:        fmt.Sprintf("%s: blocked by pattern", g.Name),
						}, nil
					}
				}
				return agents.GuardrailFunctionOutput{}, nil
			},
		}
	case "max_length":
		limit := cfg.MaxLength
		if limit <= 0 {
			limit = 50000
		}
		return &agents.InputGuardrail{
			Name: g.Name,
			Run: func(_ context.Context, _ *agents.RunContext, _ *agents.Agent, input []agents.TResponseInputItem) (agents.GuardrailFunctionOutput, error) {
				raw, _ := json.Marshal(input)
				if len(raw) > limit {
					return agents.GuardrailFunctionOutput{
						TripwireTriggered: true,
						OutputInfo:        fmt.Sprintf("Input too long: %d chars (max %d)", len(raw), limit),
					}, nil
				}
				return agents.GuardrailFunctionOutput{}, nil
			},
		}
	default:
		return nil
	}
}

func buildOutputFromDef(g *store.Guardrail) *agents.OutputGuardrail {
	var cfg store.GuardrailConfig
	_ = json.Unmarshal(g.Config, &cfg)

	switch g.Mode {
	case "regex":
		if cfg.Pattern == "" {
			return nil
		}
		re, err := regexp.Compile(cfg.Pattern)
		if err != nil {
			return nil
		}
		return &agents.OutputGuardrail{
			Name: g.Name,
			Run: func(_ context.Context, _ *agents.RunContext, _ *agents.Agent, output any) (agents.GuardrailFunctionOutput, error) {
				s := fmt.Sprintf("%v", output)
				if re.MatchString(s) {
					return agents.GuardrailFunctionOutput{
						TripwireTriggered: true,
						OutputInfo:        fmt.Sprintf("%s: blocked by pattern", g.Name),
					}, nil
				}
				return agents.GuardrailFunctionOutput{}, nil
			},
		}
	case "max_length":
		limit := cfg.MaxLength
		if limit <= 0 {
			limit = 50000
		}
		return &agents.OutputGuardrail{
			Name: g.Name,
			Run: func(_ context.Context, _ *agents.RunContext, _ *agents.Agent, output any) (agents.GuardrailFunctionOutput, error) {
				s := fmt.Sprintf("%v", output)
				if len(s) > limit {
					return agents.GuardrailFunctionOutput{
						TripwireTriggered: true,
						OutputInfo:        fmt.Sprintf("Output too long: %d chars (max %d)", len(s), limit),
					}, nil
				}
				return agents.GuardrailFunctionOutput{}, nil
			},
		}
	default:
		return nil
	}
}

// GuardrailDef describes an available guardrail for listing via the API.
type GuardrailDef struct {
	ID          string `json:"id,omitempty"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Type        string `json:"type"`
	Mode        string `json:"mode,omitempty"`
}

var builtinDefs = []GuardrailDef{
	{Name: "content_filter", Description: "Block input containing forbidden keywords/patterns", Type: "input", Mode: "regex"},
	{Name: "max_input_length", Description: "Reject input exceeding character limit (default 50000)", Type: "input", Mode: "max_length"},
	{Name: "max_output_length", Description: "Trip if output exceeds character limit (default 50000)", Type: "output", Mode: "max_length"},
}

func builtinInput(name string) *agents.InputGuardrail {
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

func builtinOutput(name string) *agents.OutputGuardrail {
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
