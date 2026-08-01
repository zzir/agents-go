package bridge

import (
	"slices"
	"strings"
	"testing"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

func TestValidateProviderSelection(t *testing.T) {
	ok := func(providerType, authMode string) *store.AgentConfig {
		ac := &store.AgentConfig{}
		ac.Provider.ProviderType = providerType
		ac.Provider.AuthMode = authMode
		return ac
	}
	for _, tc := range []struct {
		name    string
		ac      *store.AgentConfig
		wantSub string
	}{
		{"empty defaults to openai", ok("", ""), ""},
		{"openai", ok("openai", ""), ""},
		{"anthropic", ok("anthropic", ""), ""},
		{"openai chatgpt_login", ok("openai", "chatgpt_login"), ""},
		{"empty type chatgpt_login", ok("", "chatgpt_login"), ""},
		{"anthropic chatgpt_login", ok("anthropic", "chatgpt_login"), "chatgpt_login"},
		{"unknown type", ok("gemini", ""), "unknown provider"},
	} {
		err := ValidateProviderSelection(tc.ac)
		if tc.wantSub == "" {
			if err != nil {
				t.Errorf("%s: unexpected error: %v", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
			t.Errorf("%s: error = %v, want substring %q", tc.name, err, tc.wantSub)
		}
	}
}

// A fallback entry naming an unknown provider must fail at decode (= save)
// time, not run silently on OpenAI — and so must an entry with a misspelled
// selector key, which DisallowUnknownFields turns from a silent no-op into an
// error.
func TestDecodeAgentSpecValidatesFallbackProvider(t *testing.T) {
	ac := &store.AgentConfig{}
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

// ProviderTypes hands out copies: a caller mutating its result must not be
// able to poison the registry validation reads from.
func TestProviderTypesReturnsCopies(t *testing.T) {
	first := ProviderTypes()
	for i := range first {
		if len(first[i].AuthModes) > 0 {
			first[i].AuthModes[0] = "tampered"
		}
	}
	for _, info := range ProviderTypes() {
		if slices.Contains(info.AuthModes, "tampered") {
			t.Fatal("mutating a ProviderTypes result reached the registry")
		}
	}
}
