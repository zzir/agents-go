package store

import (
	"context"
	"errors"
	"testing"
)

// An agent create refuses when its provider does not exist — the atomic
// guard that closes the check-then-write window. An empty provider_id is the
// built-in default and is always allowed.
func TestCreateRefusesMissingProvider(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	id := ids(t)
	providers := NewProviderStore(db)
	if err := providers.Create(ctx, &Provider{ID: id("real"), Name: "real", Type: "openai", OwnerID: id("u")}); err != nil {
		t.Fatal(err)
	}

	agents := NewAgentConfigStore(db)
	if err := agents.Create(ctx, &AgentConfig{ID: NewID(), Name: "ghost-ref", Model: "m", ProviderID: id("ghost"), OwnerID: id("u")}); !errors.Is(err, ErrProviderRef) {
		t.Fatalf("agent with a missing provider = %v, want ErrProviderRef", err)
	}
	if err := agents.Create(ctx, &AgentConfig{ID: NewID(), Name: "default-ref", Model: "m", OwnerID: id("u")}); err != nil {
		t.Fatalf("agent on the default provider: %v", err)
	}
}

// An UPDATE that re-points a row at a provider is the same race as a create:
// the guard covers it.
func TestUpdateGuardsProviderReferences(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	id := ids(t)
	providers := NewProviderStore(db)
	if err := providers.Create(ctx, &Provider{ID: id("real"), Name: "real", Type: "openai", OwnerID: id("u")}); err != nil {
		t.Fatal(err)
	}

	agents := NewAgentConfigStore(db)
	ac := &AgentConfig{ID: NewID(), Name: "ag", Model: "m", ProviderID: id("real"), OwnerID: id("u")}
	if err := agents.Create(ctx, ac); err != nil {
		t.Fatal(err)
	}
	ac.ProviderID = id("ghost")
	if err := agents.Update(ctx, ac.ID, ac, nil); !errors.Is(err, ErrProviderRef) {
		t.Fatalf("agent update to a missing provider = %v, want ErrProviderRef", err)
	}
}

// The reference rule runs INSIDE the write transaction: a global agent (or a
// foreign private one) naming a private provider is refused at the store, so
// a scope flip cannot slip between a handler's validation and the row
// landing (decisions §5.29).
func TestWritesRefuseOutOfScopeProvider(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	id := ids(t)
	providers := NewProviderStore(db)
	private := &Provider{ID: id("pv"), Name: "pv", Type: "openai", Scope: ScopePrivate, OwnerID: id("alice")}
	if err := providers.Create(ctx, private); err != nil {
		t.Fatal(err)
	}

	agents := NewAgentConfigStore(db)
	global := &AgentConfig{ID: NewID(), Name: "g", Model: "m", ProviderID: private.ID, Scope: ScopeGlobal, OwnerID: id("admin")}
	if err := agents.Create(ctx, global); !errors.Is(err, ErrProviderScope) {
		t.Fatalf("global agent on a private provider = %v, want ErrProviderScope", err)
	}
	foreign := &AgentConfig{ID: NewID(), Name: "f", Model: "m", ProviderID: private.ID, Scope: ScopePrivate, OwnerID: id("bob")}
	if err := agents.Create(ctx, foreign); !errors.Is(err, ErrProviderScope) {
		t.Fatalf("foreign private ref = %v, want ErrProviderScope", err)
	}
	own := &AgentConfig{ID: NewID(), Name: "o", Model: "m", ProviderID: private.ID, Scope: ScopePrivate, OwnerID: id("alice")}
	if err := agents.Create(ctx, own); err != nil {
		t.Fatalf("the owner's own ref must pass: %v", err)
	}
}

// DemoteToPrivate counts and flips in one transaction: foreign references
// refuse the flip and leave the row untouched; with none, the row returns to
// its author's private set.
func TestDemoteToPrivateGuardsReferences(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	id := ids(t)
	providers := NewProviderStore(db)
	pv := &Provider{ID: id("pv"), Name: "shared", Type: "openai", Scope: ScopeGlobal, OwnerID: id("alice")}
	if err := providers.Create(ctx, pv); err != nil {
		t.Fatal(err)
	}
	agents := NewAgentConfigStore(db)
	ref := &AgentConfig{ID: NewID(), Name: "g", Model: "m", ProviderID: pv.ID, Scope: ScopeGlobal, OwnerID: id("admin")}
	if err := agents.Create(ctx, ref); err != nil {
		t.Fatal(err)
	}

	refs, err := providers.DemoteToPrivate(ctx, pv.ID)
	if err != nil || refs != 1 {
		t.Fatalf("demote with a global ref = (%d, %v), want (1, nil)", refs, err)
	}
	got, err := providers.Get(ctx, pv.ID)
	if err != nil || got.Scope != ScopeGlobal {
		t.Fatalf("refused demote must leave the row global: (%+v, %v)", got, err)
	}

	if err := agents.Delete(ctx, ref.ID); err != nil {
		t.Fatal(err)
	}
	refs, err = providers.DemoteToPrivate(ctx, pv.ID)
	if err != nil || refs != 0 {
		t.Fatalf("unblocked demote = (%d, %v), want (0, nil)", refs, err)
	}
	got, err = providers.Get(ctx, pv.ID)
	if err != nil || got.Scope != ScopePrivate || got.OwnerID != id("alice") {
		t.Fatalf("demoted row = (%s, %s), want the author's private set", got.Scope, got.OwnerID)
	}
}
