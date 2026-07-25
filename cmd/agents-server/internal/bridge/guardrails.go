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

// BuildInputGuardrails resolves a JSON array of guardrail names into input
// guardrails. A malformed list or an unknown name is a config error rather than
// a silent drop: a guardrail that appears enabled but never runs is a security
// hole, so the caller fails the build instead.
func (r *GuardrailResolver) BuildInputGuardrails(ctx context.Context, namesJSON string) ([]agents.Guardrail, error) {
	var names []string
	if namesJSON == "" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(namesJSON), &names); err != nil {
		return nil, fmt.Errorf("input_guardrails is not valid JSON: %w", err)
	}
	var out []agents.Guardrail
	for _, name := range names {
		g := r.resolveInput(ctx, name)
		if g == nil {
			return nil, fmt.Errorf("input guardrail %q not found", name)
		}
		out = append(out, *g)
	}
	return out, nil
}

// BuildOutputGuardrails resolves a JSON array of guardrail names into output
// guardrails, with the same fail-loud contract as BuildInputGuardrails.
func (r *GuardrailResolver) BuildOutputGuardrails(ctx context.Context, namesJSON string) ([]agents.Guardrail, error) {
	var names []string
	if namesJSON == "" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(namesJSON), &names); err != nil {
		return nil, fmt.Errorf("output_guardrails is not valid JSON: %w", err)
	}
	var out []agents.Guardrail
	for _, name := range names {
		g := r.resolveOutput(ctx, name)
		if g == nil {
			return nil, fmt.Errorf("output guardrail %q not found", name)
		}
		out = append(out, *g)
	}
	return out, nil
}

// ValidateGuardrailDef checks a guardrail definition at save time so a config
// that would silently no-op (empty/invalid regex, unknown mode/type) is
// rejected up front instead of failing — or resolving to "not found" — only
// when an agent later references it.
func ValidateGuardrailDef(g *store.Guardrail) error {
	switch g.Type {
	case "input", "output":
	default:
		return fmt.Errorf("type must be input or output, got %q", g.Type)
	}
	var cfg store.GuardrailConfig
	if len(g.Config) > 0 {
		if err := json.Unmarshal(g.Config, &cfg); err != nil {
			return fmt.Errorf("config is not valid JSON: %w", err)
		}
	}
	switch g.Mode {
	case "regex":
		if cfg.Pattern == "" {
			return fmt.Errorf("regex guardrail requires a non-empty config.pattern")
		}
		if _, err := regexp.Compile(cfg.Pattern); err != nil {
			return fmt.Errorf("invalid regex pattern: %w", err)
		}
	case "max_length":
		if cfg.MaxLength < 0 {
			return fmt.Errorf("max_length must not be negative")
		}
	default:
		return fmt.Errorf("mode must be regex or max_length, got %q", g.Mode)
	}
	return nil
}

// ValidateNames reports the first input/output guardrail name that is malformed
// or unresolvable, for save-time rejection of a config that would otherwise run
// unprotected.
func (r *GuardrailResolver) ValidateNames(ctx context.Context, inputJSON, outputJSON string) error {
	if _, err := r.BuildInputGuardrails(ctx, inputJSON); err != nil {
		return err
	}
	_, err := r.BuildOutputGuardrails(ctx, outputJSON)
	return err
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
				Config:      g.Config,
				Blocking:    g.Blocking,
			})
		}
	}
	all = append(all, builtinDefs...)
	return all
}

func (r *GuardrailResolver) resolveInput(ctx context.Context, name string) *agents.Guardrail {
	if r.store != nil {
		if g := r.findByName(ctx, name, "input"); g != nil {
			return buildInputFromDef(g)
		}
	}
	return builtinInput(name)
}

func (r *GuardrailResolver) resolveOutput(ctx context.Context, name string) *agents.Guardrail {
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

func buildInputFromDef(g *store.Guardrail) *agents.Guardrail {
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
		return &agents.Guardrail{
			Stages:   []agents.GuardrailStage{agents.StageInput},
			Name:     g.Name,
			Blocking: g.Blocking,
			Run: func(_ context.Context, _ *agents.RunContext, p agents.GuardrailPayload) (agents.GuardrailDecision, error) {
				input := p.Input
				for _, item := range input {
					raw, _ := json.Marshal(item)
					if re.Match(raw) {
						return agents.Trip(fmt.Sprintf("%s: blocked by pattern", g.Name)), nil
					}
				}
				return agents.Allow(nil), nil
			},
		}
	case "max_length":
		limit := cfg.MaxLength
		if limit <= 0 {
			limit = 50000
		}
		return &agents.Guardrail{
			Stages:   []agents.GuardrailStage{agents.StageInput},
			Name:     g.Name,
			Blocking: g.Blocking,
			Run: func(_ context.Context, _ *agents.RunContext, p agents.GuardrailPayload) (agents.GuardrailDecision, error) {
				input := p.Input
				raw, _ := json.Marshal(input)
				if len(raw) > limit {
					return agents.Trip(fmt.Sprintf("Input too long: %d chars (max %d)", len(raw), limit)), nil
				}
				return agents.Allow(nil), nil
			},
		}
	default:
		return nil
	}
}

func buildOutputFromDef(g *store.Guardrail) *agents.Guardrail {
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
		return &agents.Guardrail{
			Stages: []agents.GuardrailStage{agents.StageOutput},
			Name:   g.Name,
			Run: func(_ context.Context, _ *agents.RunContext, p agents.GuardrailPayload) (agents.GuardrailDecision, error) {
				output := p.Output
				s := fmt.Sprintf("%v", output)
				if re.MatchString(s) {
					return agents.Trip(fmt.Sprintf("%s: blocked by pattern", g.Name)), nil
				}
				return agents.Allow(nil), nil
			},
		}
	case "max_length":
		limit := cfg.MaxLength
		if limit <= 0 {
			limit = 50000
		}
		return &agents.Guardrail{
			Stages: []agents.GuardrailStage{agents.StageOutput},
			Name:   g.Name,
			Run: func(_ context.Context, _ *agents.RunContext, p agents.GuardrailPayload) (agents.GuardrailDecision, error) {
				output := p.Output
				s := fmt.Sprintf("%v", output)
				if len(s) > limit {
					return agents.Trip(fmt.Sprintf("Output too long: %d chars (max %d)", len(s), limit)), nil
				}
				return agents.Allow(nil), nil
			},
		}
	default:
		return nil
	}
}

// GuardrailDef describes an available guardrail for listing via the API. The
// edit form initializes from list items (the useCrud contract), so stored
// guardrails must carry every editable field here; built-in defs have fixed
// behavior and omit Config/Blocking.
type GuardrailDef struct {
	ID          string          `json:"id,omitempty"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Type        string          `json:"type"`
	Mode        string          `json:"mode,omitempty"`
	Config      json.RawMessage `json:"config,omitempty"`
	Blocking    bool            `json:"blocking,omitempty"`
}

var builtinDefs = []GuardrailDef{
	{Name: "content_filter", Description: "Block input containing forbidden keywords/patterns", Type: "input", Mode: "regex"},
	{Name: "max_input_length", Description: "Reject input exceeding character limit (default 50000)", Type: "input", Mode: "max_length"},
	{Name: "max_output_length", Description: "Trip if output exceeds character limit (default 50000)", Type: "output", Mode: "max_length"},
}

func builtinInput(name string) *agents.Guardrail {
	switch name {
	case "content_filter":
		return &agents.Guardrail{
			Stages: []agents.GuardrailStage{agents.StageInput},
			Name:   "content_filter",
			Run: func(_ context.Context, _ *agents.RunContext, p agents.GuardrailPayload) (agents.GuardrailDecision, error) {
				input := p.Input
				forbidden := regexp.MustCompile(`(?i)(ignore previous instructions|system prompt|jailbreak)`)
				for _, item := range input {
					raw, _ := json.Marshal(item)
					if forbidden.Match(raw) {
						return agents.Trip("Content filter: potentially harmful input detected"), nil
					}
				}
				return agents.Allow(nil), nil
			},
		}
	case "max_input_length":
		return &agents.Guardrail{
			Stages: []agents.GuardrailStage{agents.StageInput},
			Name:   "max_input_length",
			Run: func(_ context.Context, _ *agents.RunContext, p agents.GuardrailPayload) (agents.GuardrailDecision, error) {
				input := p.Input
				raw, _ := json.Marshal(input)
				if len(raw) > 50000 {
					return agents.Trip(fmt.Sprintf("Input too long: %d chars (max 50000)", len(raw))), nil
				}
				return agents.Allow(nil), nil
			},
		}
	default:
		return nil
	}
}

func builtinOutput(name string) *agents.Guardrail {
	switch name {
	case "max_output_length":
		return &agents.Guardrail{
			Stages: []agents.GuardrailStage{agents.StageOutput},
			Name:   "max_output_length",
			Run: func(_ context.Context, _ *agents.RunContext, p agents.GuardrailPayload) (agents.GuardrailDecision, error) {
				output := p.Output
				s := fmt.Sprintf("%v", output)
				if len(s) > 50000 {
					return agents.Trip(fmt.Sprintf("Output too long: %d chars (max 50000)", len(s))), nil
				}
				return agents.Allow(nil), nil
			},
		}
	default:
		return nil
	}
}
