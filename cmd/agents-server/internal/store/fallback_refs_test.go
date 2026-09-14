package store

import (
	"context"
	"errors"
	"testing"
)

// A provider named only by a fallback entry is a reference like the primary:
// the delete guard counts it, and an agent write re-checks it in the transaction.
func TestFallbackEntriesAreProviderReferences(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	id := ids(t)
	providers := NewProviderStore(db)
	agents := NewAgentConfigStore(db)
	if err := providers.Create(ctx, &Provider{ID: id("spare"), Name: "spare", Type: "openai", OwnerID: id("u"), Scope: ScopePrivate}); err != nil {
		t.Fatal(err)
	}
	if err := providers.Create(ctx, &Provider{ID: id("theirs"), Name: "theirs", Type: "openai", OwnerID: id("v"), Scope: ScopePrivate}); err != nil {
		t.Fatal(err)
	}

	ac := &AgentConfig{ID: NewID(), Name: "with-fallback", Model: "m", OwnerID: id("u"), Scope: ScopePrivate,
		Resilience: ResilienceGroup{FallbackModels: FallbackModels{{ProviderID: id("spare"), Model: "alt"}}}}
	if err := agents.Create(ctx, ac); err != nil {
		t.Fatal(err)
	}
	refs, err := providers.DeleteIfUnreferenced(ctx, id("spare"), id("u"))
	if err != nil || refs != 1 {
		t.Fatalf("delete of a fallback-only provider: refs=%d err=%v, want refused by one reference", refs, err)
	}
	if _, err := providers.Get(ctx, id("spare")); err != nil {
		t.Fatalf("the provider must survive: %v", err)
	}

	ghost := &AgentConfig{ID: NewID(), Name: "ghost-fallback", Model: "m", OwnerID: id("u"),
		Resilience: ResilienceGroup{FallbackModels: FallbackModels{{ProviderID: id("ghost")}}}}
	if err := agents.Create(ctx, ghost); !errors.Is(err, ErrProviderRef) {
		t.Fatalf("a fallback naming no provider = %v, want ErrProviderRef", err)
	}
	foreign := &AgentConfig{ID: NewID(), Name: "foreign-fallback", Model: "m", OwnerID: id("u"), Scope: ScopeGlobal,
		Resilience: ResilienceGroup{FallbackModels: FallbackModels{{ProviderID: id("theirs")}}}}
	if err := agents.Create(ctx, foreign); !errors.Is(err, ErrProviderScope) {
		t.Fatalf("a global agent on another user's private fallback = %v, want ErrProviderScope", err)
	}
}
