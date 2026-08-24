package providers

import (
	"slices"
	"strings"
	"testing"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

func TestValidateProvider(t *testing.T) {
	ok := func(providerType, authMode string) *store.Provider {
		return &store.Provider{Type: providerType, AuthMode: authMode}
	}
	withBase := func(providerType, authMode, baseURL string) *store.Provider {
		return &store.Provider{Type: providerType, AuthMode: authMode, BaseURL: baseURL}
	}
	for _, tc := range []struct {
		name    string
		pv      *store.Provider
		wantSub string
	}{
		{"empty defaults to openai", ok("", ""), ""},
		{"openai", ok("openai", ""), ""},
		{"anthropic", ok("anthropic", ""), ""},
		{"openai chatgpt_login", ok("openai", "chatgpt_login"), ""},
		{"empty type chatgpt_login", ok("", "chatgpt_login"), ""},
		{"anthropic chatgpt_login", ok("anthropic", "chatgpt_login"), "chatgpt_login"},
		{"unknown type", ok("gemini", ""), "unknown provider"},
		// The OAuth token is only ever sent to ChatGPT, so a custom base_url is
		// refused with chatgpt_login — but fine with a plain API key.
		{"chatgpt_login + custom base_url", withBase("openai", "chatgpt_login", "https://evil.test"), "base_url cannot be set"},
		{"api key + custom base_url", withBase("openai", "", "https://proxy.internal"), ""},
	} {
		err := Validate(tc.pv)
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

// Types hands out copies: a caller mutating its result must not be
// able to poison the registry validation reads from.
func TestProviderTypesReturnsCopies(t *testing.T) {
	first := Types()
	for i := range first {
		if len(first[i].AuthModes) > 0 {
			first[i].AuthModes[0] = "tampered"
		}
	}
	for _, info := range Types() {
		if slices.Contains(info.AuthModes, "tampered") {
			t.Fatal("mutating a Types result reached the registry")
		}
	}
}
