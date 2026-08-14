package store

import (
	"context"
	"errors"
	"testing"
)

// A route or agent create refuses when its provider does not exist — the atomic
// guard that closes the check-then-write window. An empty provider_id is the
// built-in default and is always allowed.
func TestCreateRefusesMissingProvider(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	providers := NewProviderStore(db)
	if err := providers.Create(ctx, &Provider{ID: "real", Name: "real", Type: "openai"}); err != nil {
		t.Fatal(err)
	}

	routes := NewProviderRouteStore(db)
	if err := routes.Create(ctx, &ProviderRoute{ID: NewID(), Prefix: "a", ProviderID: "ghost"}); !errors.Is(err, ErrProviderRef) {
		t.Fatalf("route with a missing provider = %v, want ErrProviderRef", err)
	}
	if err := routes.Create(ctx, &ProviderRoute{ID: NewID(), Prefix: "b", ProviderID: "real"}); err != nil {
		t.Fatalf("route with a real provider: %v", err)
	}

	agents := NewAgentConfigStore(db)
	if err := agents.Create(ctx, &AgentConfig{ID: NewID(), Name: "ghost-ref", Model: "m", ProviderID: "ghost"}); !errors.Is(err, ErrProviderRef) {
		t.Fatalf("agent with a missing provider = %v, want ErrProviderRef", err)
	}
	if err := agents.Create(ctx, &AgentConfig{ID: NewID(), Name: "default-ref", Model: "m"}); err != nil {
		t.Fatalf("agent on the default provider: %v", err)
	}
}

// An UPDATE that re-points a row at a provider is the same race as a create:
// the guard covers it, and a route can never be re-pointed at a chatgpt_login
// provider.
func TestUpdateGuardsProviderReferences(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	providers := NewProviderStore(db)
	for _, p := range []*Provider{
		{ID: "real", Name: "real", Type: "openai"},
		{ID: "chatgpt", Name: "chatgpt", Type: "openai", AuthMode: AuthModeChatGPTLogin},
	} {
		if err := providers.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
	}

	routes := NewProviderRouteStore(db)
	route := &ProviderRoute{ID: NewID(), Prefix: "a", ProviderID: "real"}
	if err := routes.Create(ctx, route); err != nil {
		t.Fatal(err)
	}
	route.ProviderID = "ghost"
	if err := routes.Update(ctx, route.ID, route); !errors.Is(err, ErrProviderRef) {
		t.Fatalf("route update to a missing provider = %v, want ErrProviderRef", err)
	}
	route.ProviderID = "chatgpt"
	if err := routes.Update(ctx, route.ID, route); !errors.Is(err, ErrProviderNotRoutable) {
		t.Fatalf("route update to a chatgpt_login provider = %v, want ErrProviderNotRoutable", err)
	}

	agents := NewAgentConfigStore(db)
	ac := &AgentConfig{ID: NewID(), Name: "ag", Model: "m", ProviderID: "real"}
	if err := agents.Create(ctx, ac); err != nil {
		t.Fatal(err)
	}
	ac.ProviderID = "ghost"
	if err := agents.Update(ctx, ac.ID, ac); !errors.Is(err, ErrProviderRef) {
		t.Fatalf("agent update to a missing provider = %v, want ErrProviderRef", err)
	}
}
