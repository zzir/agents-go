package bridge

import (
	"testing"

	"github.com/zzir/agents-go/agents"
)

// recordingProvider records the model name it is asked for.
type recordingProvider struct{ got *string }

func (r recordingProvider) Model(name string) (agents.Model, error) {
	*r.got = name
	return nil, nil
}

// fixedModelProvider must pin the provider to the CONFIGURED model name,
// ignoring the name the run requests — otherwise a fallback_models[].model is
// silently ignored because the SDK asks every fallback for the primary's name.
func TestFixedModelProviderUsesConfiguredModel(t *testing.T) {
	var got string
	fp := fixedModelProvider{inner: recordingProvider{got: &got}, model: "gpt-4o-mini"}
	if _, err := fp.Model("gpt-4o"); err != nil {
		t.Fatalf("Model: %v", err)
	}
	if got != "gpt-4o-mini" {
		t.Fatalf("fallback provider was asked for %q, want the configured gpt-4o-mini", got)
	}
}
