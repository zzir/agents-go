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

// BuildGuardrails resolves a JSON array of guardrail names into SDK guardrails.
// A malformed list or an unknown name is a config error rather than a silent
// drop: a guardrail that appears enabled but never runs is a security hole, so
// the caller fails the build instead.
func (r *GuardrailResolver) BuildGuardrails(ctx context.Context, namesJSON string) ([]agents.Guardrail, error) {
	var names []string
	if namesJSON == "" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(namesJSON), &names); err != nil {
		return nil, fmt.Errorf("guardrails is not valid JSON: %w", err)
	}
	var out []agents.Guardrail
	for _, name := range names {
		g := r.resolve(ctx, name)
		if g == nil {
			return nil, fmt.Errorf("guardrail %q not found", name)
		}
		out = append(out, *g)
	}
	return out, nil
}

// ValidateGuardrailDef checks a guardrail definition at save time so a config
// that would silently no-op (empty/invalid regex, unknown mode, no stages) is
// rejected up front instead of failing — or resolving to "not found" — only
// when an agent later references it.
func ValidateGuardrailDef(g *store.Guardrail) error {
	if len(g.Stages) == 0 {
		return fmt.Errorf("at least one stage is required")
	}
	for _, st := range g.Stages {
		if !validStage(st) {
			return fmt.Errorf("unknown stage %q (want input, output, tool_input or tool_output)", st)
		}
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

func validStage(s string) bool {
	switch agents.GuardrailStage(s) {
	case agents.StageInput, agents.StageOutput, agents.StageToolInput, agents.StageToolOutput:
		return true
	}
	return false
}

// ValidateNames reports the first guardrail name that is malformed or
// unresolvable, for save-time rejection of a config that would otherwise run
// unprotected.
func (r *GuardrailResolver) ValidateNames(ctx context.Context, namesJSON string) error {
	_, err := r.BuildGuardrails(ctx, namesJSON)
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
				Stages:      g.Stages,
				Mode:        g.Mode,
				Config:      g.Config,
				Blocking:    g.Blocking,
			})
		}
	}
	all = append(all, builtinDefs...)
	return all
}

func (r *GuardrailResolver) resolve(ctx context.Context, name string) *agents.Guardrail {
	if r.store != nil {
		if g := r.findByName(ctx, name); g != nil {
			return buildFromDef(g)
		}
	}
	return builtin(name)
}

func (r *GuardrailResolver) findByName(ctx context.Context, name string) *store.Guardrail {
	all, err := r.store.List(ctx)
	if err != nil {
		return nil
	}
	for i := range all {
		if all[i].Name == name {
			return &all[i]
		}
	}
	return nil
}

// inspected returns the text a guardrail examines at the stage it was invoked
// at. One definition covering several stages is the SDK's model — a content
// scanner that should see the input, the tool arguments and the final output is
// one guardrail with three stages, not three near-identical copies, which is
// what this server had.
func inspected(p agents.GuardrailPayload) string {
	switch p.Stage {
	case agents.StageInput:
		raw, _ := json.Marshal(p.Input)
		return string(raw)
	case agents.StageToolInput:
		return p.Arguments
	default: // StageOutput, StageToolOutput
		return fmt.Sprintf("%v", p.Output)
	}
}

// stagesOf converts stored stage names to the SDK's, dropping any this build
// does not know rather than failing — the definition was validated on save, so
// an unknown one here means a newer server wrote it.
func stagesOf(names []string) []agents.GuardrailStage {
	out := make([]agents.GuardrailStage, 0, len(names))
	for _, n := range names {
		if validStage(n) {
			out = append(out, agents.GuardrailStage(n))
		}
	}
	return out
}

func buildFromDef(g *store.Guardrail) *agents.Guardrail {
	var cfg store.GuardrailConfig
	_ = json.Unmarshal(g.Config, &cfg)

	stages := stagesOf(g.Stages)
	if len(stages) == 0 {
		return nil
	}

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
			Stages:   stages,
			Name:     g.Name,
			Blocking: g.Blocking,
			Run: func(_ context.Context, _ *agents.RunContext, p agents.GuardrailPayload) (agents.GuardrailDecision, error) {
				if re.MatchString(inspected(p)) {
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
			Stages:   stages,
			Name:     g.Name,
			Blocking: g.Blocking,
			Run: func(_ context.Context, _ *agents.RunContext, p agents.GuardrailPayload) (agents.GuardrailDecision, error) {
				if n := len(inspected(p)); n > limit {
					return agents.Trip(fmt.Sprintf("%s: %d chars exceeds the %d limit at stage %s", g.Name, n, limit, p.Stage)), nil
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
	Stages      []string        `json:"stages"`
	Mode        string          `json:"mode,omitempty"`
	Config      json.RawMessage `json:"config,omitempty"`
	Blocking    bool            `json:"blocking,omitempty"`
}

var builtinDefs = []GuardrailDef{
	{Name: "content_filter", Description: "Block prompt-injection phrasing in input and tool arguments", Stages: []string{"input", "tool_input"}, Mode: "regex"},
	{Name: "max_input_length", Description: "Reject input exceeding character limit (default 50000)", Stages: []string{"input"}, Mode: "max_length"},
	{Name: "max_output_length", Description: "Trip if output exceeds character limit (default 50000)", Stages: []string{"output"}, Mode: "max_length"},
}

func builtin(name string) *agents.Guardrail {
	switch name {
	case "content_filter":
		// Also at the tool-input stage: the phrasing this looks for is just as
		// dangerous arriving in a tool's arguments, and the SDK models one
		// guardrail covering both rather than two copies of it.
		return &agents.Guardrail{
			Stages: []agents.GuardrailStage{agents.StageInput, agents.StageToolInput},
			Name:   "content_filter",
			Run: func(_ context.Context, _ *agents.RunContext, p agents.GuardrailPayload) (agents.GuardrailDecision, error) {
				forbidden := regexp.MustCompile(`(?i)(ignore previous instructions|system prompt|jailbreak)`)
				if forbidden.MatchString(inspected(p)) {
					return agents.Trip("Content filter: potentially harmful input detected"), nil
				}
				return agents.Allow(nil), nil
			},
		}
	case "max_input_length":
		return &agents.Guardrail{
			Stages: []agents.GuardrailStage{agents.StageInput},
			Name:   "max_input_length",
			Run: func(_ context.Context, _ *agents.RunContext, p agents.GuardrailPayload) (agents.GuardrailDecision, error) {
				if n := len(inspected(p)); n > 50000 {
					return agents.Trip(fmt.Sprintf("Input too long: %d chars (max 50000)", n)), nil
				}
				return agents.Allow(nil), nil
			},
		}
	case "max_output_length":
		return &agents.Guardrail{
			Stages: []agents.GuardrailStage{agents.StageOutput},
			Name:   "max_output_length",
			Run: func(_ context.Context, _ *agents.RunContext, p agents.GuardrailPayload) (agents.GuardrailDecision, error) {
				if n := len(inspected(p)); n > 50000 {
					return agents.Trip(fmt.Sprintf("Output too long: %d chars (max 50000)", n)), nil
				}
				return agents.Allow(nil), nil
			},
		}
	default:
		return nil
	}
}
