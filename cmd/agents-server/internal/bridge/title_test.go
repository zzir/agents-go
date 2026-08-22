package bridge

import (
	"context"
	"testing"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// The guards that keep parallel title generation from misfiring: it must not
// rename a session that already has a title, nor make a model call without a
// user message or a provider.
func TestMaybeGenerateTitleGuards(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessions := store.NewSessionStore(db)
	runner := NewRunner(ctx, db, &AgentDeps{Sessions: sessions})

	// Already-titled session: no-op even with a user message (name guard runs
	// first, before touching a model).
	titled := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "Existing Title"}
	if err := sessions.Create(ctx, titled); err != nil {
		t.Fatalf("create titled session: %v", err)
	}
	runner.maybeGenerateTitle(ctx, titled.ID, "gpt-test", "hello there", nil, func(string, any) {})
	if got, _ := sessions.Get(ctx, titled.ID); got.Name != "Existing Title" {
		t.Errorf("titled session renamed to %q", got.Name)
	}

	// New Session but no user input: no model call, no rename.
	empty := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "New Session"}
	if err := sessions.Create(ctx, empty); err != nil {
		t.Fatalf("create empty session: %v", err)
	}
	runner.maybeGenerateTitle(ctx, empty.ID, "gpt-test", "", nil, func(string, any) {})
	if got, _ := sessions.Get(ctx, empty.ID); got.Name != "New Session" {
		t.Errorf("empty New Session renamed to %q", got.Name)
	}

	// New Session with input but no provider: still bails before any model call.
	noProv := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "New Session"}
	if err := sessions.Create(ctx, noProv); err != nil {
		t.Fatalf("create no-provider session: %v", err)
	}
	runner.maybeGenerateTitle(ctx, noProv.ID, "gpt-test", "a question", nil, func(string, any) {})
	if got, _ := sessions.Get(ctx, noProv.ID); got.Name != "New Session" {
		t.Errorf("no-provider New Session renamed to %q", got.Name)
	}
}
