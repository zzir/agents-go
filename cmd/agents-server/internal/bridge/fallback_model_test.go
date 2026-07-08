package bridge

import (
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// recordingProvider records the model name it is asked for.
type recordingProvider struct{ got *string }

func (r recordingProvider) GetModel(name string) (agents.Model, error) {
	*r.got = name
	return nil, nil
}

// fixedModelProvider must pin the provider to the CONFIGURED model name,
// ignoring the name the run requests — otherwise a fallback_models[].model is
// silently ignored because the SDK asks every fallback for the primary's name.
func TestFixedModelProviderUsesConfiguredModel(t *testing.T) {
	var got string
	fp := fixedModelProvider{inner: recordingProvider{got: &got}, model: "gpt-4o-mini"}
	if _, err := fp.GetModel("gpt-4o"); err != nil {
		t.Fatalf("GetModel: %v", err)
	}
	if got != "gpt-4o-mini" {
		t.Fatalf("fallback provider was asked for %q, want the configured gpt-4o-mini", got)
	}
}

// DecodeAgentSpec fails on malformed fallback_models instead of silently
// dropping it (which would look like fallback is configured but do nothing).
func TestDecodeAgentSpecFallbackMalformedFails(t *testing.T) {
	_, err := DecodeAgentSpec(&store.AgentConfig{Name: "a", Resilience: store.ResilienceGroup{FallbackModels: "{not json"}})
	if err == nil {
		t.Fatal("malformed fallback_models must fail, not silently drop fallback")
	}
}
