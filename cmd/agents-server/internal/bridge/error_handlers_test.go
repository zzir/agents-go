package bridge

import (
	"context"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// Malformed or mistyped error_handlers config must fail the save/build loudly:
// a fallback that "looks configured" but silently never fires would leave the
// operator believing failures are handled.
func TestDecodeErrorHandlersValidation(t *testing.T) {
	structured, err := BuildOutputSchema(`{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]}`)
	if err != nil {
		t.Fatalf("schema: %v", err)
	}

	cases := []struct {
		name       string
		raw        string
		outputType agents.OutputSchema
		wantSub    string // "" means expect success
	}{
		{"not json", "{bad", nil, "error_handlers is invalid"},
		{"unknown kind key", `{"max_turn":{"final_output":"x"}}`, nil, "unknown field"},
		{"missing final_output", `{"max_turns":{"exclude_from_history":true}}`, nil, "final_output is required"},
		{"plain text agent rejects object fallback", `{"max_turns":{"final_output":{"a":1}}}`, nil, "must be a JSON string"},
		{"plain text agent accepts string", `{"max_turns":{"final_output":"budget spent"}}`, nil, ""},
		{"structured agent accepts object", `{"invalid_final_output":{"final_output":{"summary":"fallback"}}}`, structured, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeErrorHandlers(tc.raw, tc.outputType)
			if tc.wantSub == "" {
				if err != nil {
					t.Fatalf("want success, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("want error containing %q, got %v", tc.wantSub, err)
			}
		})
	}
}

// BuildFullAgent must translate the declarative config into working run-level
// handlers: configured kinds recover with their static fallback, unconfigured
// kinds stay nil (fatal).
func TestBuildFullAgentBuildsErrorHandlers(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	s := store.NewAgentConfigStore(db)
	deps := &AgentDeps{
		AgentConfigs: s,
		Settings:     settings.NewReader(store.NewSettingStore(db)),
		Memories:     store.NewMemoryStore(db),
	}

	ac := &store.AgentConfig{
		Name:  "recovering",
		Model: "gpt-test",
		ErrorHandlers: `{
			"max_turns": {"final_output": "budget spent", "exclude_from_history": true},
			"model_refusal": {"final_output": "refused politely"}
		}`,
	}
	if err := s.Create(ctx, ac); err != nil {
		t.Fatalf("create: %v", err)
	}
	built, err := BuildFullAgent(ctx, deps, ac.ID, "", store.LocalUserID)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	if built.ErrorHandlers.InvalidFinalOutput != nil {
		t.Error("unconfigured invalid_final_output handler should be nil")
	}
	if built.ErrorHandlers.MaxTurns == nil || built.ErrorHandlers.ModelRefusal == nil {
		t.Fatal("configured handlers should be non-nil")
	}

	res, err := built.ErrorHandlers.MaxTurns(ctx, agents.RunErrorHandlerInput{})
	if err != nil || res == nil {
		t.Fatalf("max_turns handler = (%v, %v)", res, err)
	}
	if res.FinalOutput != "budget spent" || !res.ExcludeFromHistory {
		t.Errorf("max_turns fallback = %#v", res)
	}

	res, err = built.ErrorHandlers.ModelRefusal(ctx, agents.RunErrorHandlerInput{})
	if err != nil || res == nil || res.FinalOutput != "refused politely" || res.ExcludeFromHistory {
		t.Errorf("model_refusal fallback = %#v (err %v)", res, err)
	}
}

// A structured agent's object fallback survives the decode→handler round trip
// as a plain Go value the SDK can validate against the output schema.
func TestBuildErrorHandlersStructuredFallback(t *testing.T) {
	spec, err := decodeErrorHandlers(
		`{"invalid_final_output":{"final_output":{"summary":"fallback","ok":true}}}`,
		agents.NewDynamicOutputSchema("final_output", map[string]any{"type": "object"}, false),
	)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	handlers := spec.BuildErrorHandlers()
	res, err := handlers.InvalidFinalOutput(context.Background(), agents.RunErrorHandlerInput{})
	if err != nil || res == nil {
		t.Fatalf("handler = (%v, %v)", res, err)
	}
	m, ok := res.FinalOutput.(map[string]any)
	if !ok || m["summary"] != "fallback" || m["ok"] != true {
		t.Errorf("fallback = %#v", res.FinalOutput)
	}
}

// Bad error_handlers config is rejected at build (and thus save) time via
// DecodeAgentSpec, like every other JSON-encoded config field.
func TestBuildFullAgentFailsOnBadErrorHandlers(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	s := store.NewAgentConfigStore(db)
	deps := &AgentDeps{
		AgentConfigs: s,
		Settings:     settings.NewReader(store.NewSettingStore(db)),
		Memories:     store.NewMemoryStore(db),
	}

	ac := &store.AgentConfig{
		Name:          "broken",
		Model:         "gpt-test",
		ErrorHandlers: `{"max_turns":{"final_output":{"not":"a string"}}}`, // plain-text agent
	}
	if err := s.Create(ctx, ac); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err := BuildFullAgent(ctx, deps, ac.ID, "", store.LocalUserID)
	if err == nil || !strings.Contains(err.Error(), "must be a JSON string") {
		t.Fatalf("want plain-text string error, got %v", err)
	}
}
