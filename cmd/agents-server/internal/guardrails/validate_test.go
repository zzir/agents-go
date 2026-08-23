package guardrails

import (
	"context"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// A guardrail definition must be validated at save time: an unknown stage or
// mode, no stages at all, and an empty or uncompilable regex are rejected up
// front — not left to fail, or resolve to "not found", when an agent later
// references the guardrail.
func TestValidateGuardrailDef(t *testing.T) {
	cases := []struct {
		name    string
		g       store.Guardrail
		wantSub string // "" = valid
	}{
		{"valid regex", store.Guardrail{Name: "n", Stages: []string{"input"}, Mode: "regex", Config: []byte(`{"pattern":"\\bx\\b"}`)}, ""},
		{"valid max_length", store.Guardrail{Name: "n", Stages: []string{"output"}, Mode: "max_length", Config: []byte(`{"max_length":100}`)}, ""},
		{"every stage at once", store.Guardrail{Name: "n", Stages: []string{"input", "output", "tool_input", "tool_output"}, Mode: "max_length"}, ""},
		{"max_length default ok", store.Guardrail{Name: "n", Stages: []string{"input"}, Mode: "max_length"}, ""},
		{"unknown stage", store.Guardrail{Name: "n", Stages: []string{"sideways"}, Mode: "regex", Config: []byte(`{"pattern":"x"}`)}, "unknown stage"},
		{"no stages", store.Guardrail{Name: "n", Mode: "regex", Config: []byte(`{"pattern":"x"}`)}, "at least one stage"},
		{"unknown mode", store.Guardrail{Name: "n", Stages: []string{"input"}, Mode: "banana"}, "mode must be"},
		{"empty regex", store.Guardrail{Name: "n", Stages: []string{"input"}, Mode: "regex"}, "non-empty config.pattern"},
		{"bad regex", store.Guardrail{Name: "n", Stages: []string{"input"}, Mode: "regex", Config: []byte(`{"pattern":"("}`)}, "invalid regex"},
		{"bad config json", store.Guardrail{Name: "n", Stages: []string{"input"}, Mode: "regex", Config: []byte(`{bad`)}, "not valid JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateDef(&tc.g)
			if tc.wantSub == "" {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("want error containing %q, got %v", tc.wantSub, err)
			}
		})
	}
}

// One definition, several stages: the same guardrail must inspect whatever the
// stage it was invoked at puts under it. Getting this wrong is silent — a
// tool_input guardrail reading p.Input sees an empty run input and allows
// everything.
func TestGuardrailInspectsTheStageItRanAt(t *testing.T) {
	g := buildFromDef(&store.Guardrail{
		Name:   "scanner",
		Stages: []string{"input", "output", "tool_input", "tool_output"},
		Mode:   "regex",
		Config: []byte(`{"pattern":"secret"}`),
	})
	if g == nil {
		t.Fatal("buildFromDef returned nil for a valid multi-stage definition")
	}
	if len(g.Stages) != 4 {
		t.Fatalf("stages = %v, want all four", g.Stages)
	}

	item, err := session.UnmarshalInputItem([]byte(`{"role":"user","content":"the secret"}`))
	if err != nil {
		t.Fatalf("build item: %v", err)
	}
	cases := []struct {
		name string
		p    agents.GuardrailPayload
		trip bool
	}{
		{"input trips", agents.GuardrailPayload{Stage: agents.StageInput, Input: []agents.InputItem{item}}, true},
		{"input allows", agents.GuardrailPayload{Stage: agents.StageInput}, false},
		{"tool arguments trip", agents.GuardrailPayload{Stage: agents.StageToolInput, Arguments: `{"q":"secret"}`}, true},
		{"tool arguments allow", agents.GuardrailPayload{Stage: agents.StageToolInput, Arguments: `{"q":"weather"}`}, false},
		{"tool result trips", agents.GuardrailPayload{Stage: agents.StageToolOutput, Output: "a secret value"}, true},
		{"final output trips", agents.GuardrailPayload{Stage: agents.StageOutput, Output: "the secret"}, true},
		{"final output allows", agents.GuardrailPayload{Stage: agents.StageOutput, Output: "all clear"}, false},
		// The run input is NOT what a tool stage inspects: reading the wrong
		// field is how a tool guardrail silently passes everything.
		{"tool stage ignores the run input", agents.GuardrailPayload{Stage: agents.StageToolInput, Input: []agents.InputItem{item}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := g.Run(context.Background(), nil, tc.p)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if tripped := d.Action == agents.GuardrailTrip; tripped != tc.trip {
				t.Errorf("tripped = %v, want %v", tripped, tc.trip)
			}
		})
	}
}

// A stored definition naming no stage this build knows would otherwise produce
// a guardrail that runs nowhere — which reads as "enabled" and protects
// nothing. Refusing to build it makes the agent fail to start instead.
func TestBuildFromDefRejectsAnUnknownStage(t *testing.T) {
	if g := buildFromDef(&store.Guardrail{
		Name: "n", Stages: []string{"sideways"}, Mode: "max_length",
	}); g != nil {
		t.Errorf("built a guardrail with no usable stage: %v", g.Stages)
	}
}
