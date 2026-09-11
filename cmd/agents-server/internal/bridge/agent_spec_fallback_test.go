package bridge

import (
	"context"
	"strings"
	"testing"

	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// A fallback entry names a provider by id, under the same reference rule as
// the primary; an entry from before provider_id names an endpoint and
// resolves to a provider the agent may reference at it. Anything else fails
// the build, never runs keyless on a default backend — decisions §5.69.
func TestFallbackProvidersResolveByIDOrEndpoint(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	providers := store.NewProviderStore(db)
	deps := &AgentDeps{Providers: providers, Settings: settings.NewReader(store.NewSettingStore(db))}
	mk := func(name, typ, baseURL, key, scope, owner string) *store.Provider {
		t.Helper()
		pv := &store.Provider{Name: name, Type: typ, BaseURL: baseURL, APIKey: key, Scope: scope, OwnerID: owner}
		if err := providers.Create(ctx, pv); err != nil {
			t.Fatal(err)
		}
		return pv
	}
	primary := mk("primary", "openai", "", "sk-p", store.ScopePrivate, store.LocalUserID)
	keyed := mk("anthropic", "anthropic", "", "sk-a", store.ScopeGlobal, store.LocalUserID)
	foreign := mk("theirs", "openai", "https://x.example/v1", "sk-x", store.ScopePrivate, store.NewID())
	keyless := mk("keyless", "openai", "https://k.example/v1", "", store.ScopeGlobal, store.LocalUserID)
	ac := &store.AgentConfig{OwnerID: store.LocalUserID, Name: "a", Model: "m", ProviderID: primary.ID, Scope: store.ScopePrivate}

	resolve := func(entries ...store.FallbackModel) error {
		t.Helper()
		ac.Resilience.FallbackModels = entries
		spec, err := DecodeAgentSpec(ac)
		if err != nil {
			return err
		}
		_, _, err = resolveProvider(ctx, deps, ac, spec, nil)
		return err
	}
	if err := resolve(store.FallbackModel{ProviderID: keyed.ID, Model: "claude"}); err != nil {
		t.Fatalf("a visible keyed provider: %v", err)
	}
	if err := resolve(store.FallbackModel{ProviderID: foreign.ID}); err == nil || !strings.Contains(err.Error(), "scope") {
		t.Fatalf("another user's private provider = %v, want a scope refusal", err)
	}
	if err := resolve(store.FallbackModel{ProviderID: store.NewID()}); err == nil || !strings.Contains(err.Error(), "fallback_models[0]") {
		t.Fatalf("a provider that does not exist = %v, want a loud failure", err)
	}
	if err := resolve(store.FallbackModel{ProviderID: keyless.ID}); err == nil || !strings.Contains(err.Error(), "no API key") {
		t.Fatalf("a keyless provider = %v, want a refusal", err)
	}
	// The endpoint form resolves to a provider the agent may reference at it
	// ("" and "openai" are one backend, a trailing slash the same host).
	if err := resolve(store.FallbackModel{ProviderType: "", BaseURL: "", Model: "gpt-x"}); err != nil {
		t.Fatalf("an endpoint with a visible provider: %v", err)
	}
	if err := resolve(store.FallbackModel{ProviderType: "openai", BaseURL: "https://x.example/v1/"}); err == nil || !strings.Contains(err.Error(), "no provider at") {
		t.Fatalf("an endpoint only another user's provider reaches = %v, want a refusal naming the endpoint", err)
	}
}
