package bridge

import (
	"strings"
	"testing"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// A fallback entry naming an unknown provider must fail at decode (= save)
// time, not run silently on OpenAI — and so must an entry with a misspelled
// selector key, which DisallowUnknownFields turns from a silent no-op into an
// error.
func TestDecodeAgentSpecValidatesFallbackProvider(t *testing.T) {
	ac := &store.AgentConfig{OwnerID: store.LocalUserID}
	ac.Resilience.FallbackModels = `[{"model":"claude-opus-5","provider_type":"anthropic"}]`
	if _, err := DecodeAgentSpec(ac); err != nil {
		t.Fatalf("valid provider rejected: %v", err)
	}
	ac.Resilience.FallbackModels = `[{"model":"m","provider_type":"gemini"}]`
	_, err := DecodeAgentSpec(ac)
	if err == nil || !strings.Contains(err.Error(), "fallback_models[0].provider_type") {
		t.Fatalf("error = %v, want fallback_models[0].provider_type complaint", err)
	}
	// The old/misspelled key: rejected loudly instead of running the entry on
	// the default backend.
	ac.Resilience.FallbackModels = `[{"model":"claude-opus-5","provider":"anthropic"}]`
	_, err = DecodeAgentSpec(ac)
	if err == nil || !strings.Contains(err.Error(), `unknown field "provider"`) {
		t.Fatalf("error = %v, want unknown-field complaint naming the key", err)
	}
	// Trailing garbage after the array: Decode (unlike Unmarshal) stops at the
	// first value, so the guard has to catch it explicitly.
	ac.Resilience.FallbackModels = `[{"model":"m"}] {"stray":true}`
	_, err = DecodeAgentSpec(ac)
	if err == nil || !strings.Contains(err.Error(), "trailing data") {
		t.Fatalf("error = %v, want trailing-data complaint", err)
	}
}
