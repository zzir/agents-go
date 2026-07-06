package bridge

import (
	"context"
	"errors"
	"testing"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// IsLoggedIn / StartLogin must distinguish a missing agent (ErrNotFound → the
// handler answers 404) from an existing agent that simply isn't logged in, so
// the ChatGPT OAuth endpoints have consistent resource semantics.
func TestChatGPTOAuthMissingAgent(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	agents := store.NewAgentConfigStore(db)
	o := NewChatGPTOAuth(agents)

	// Missing agent -> ErrNotFound, not a folded logged_in:false.
	if _, err := o.IsLoggedIn(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("IsLoggedIn(missing) err = %v, want ErrNotFound", err)
	}
	if _, err := o.StartLogin(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("StartLogin(missing) err = %v, want ErrNotFound", err)
	}

	// Existing agent with no token -> (false, nil), a real "not logged in".
	ac := &store.AgentConfig{Name: "a", Model: "m"}
	if err := agents.Create(ctx, ac); err != nil {
		t.Fatalf("create: %v", err)
	}
	loggedIn, err := o.IsLoggedIn(ctx, ac.ID)
	if err != nil {
		t.Fatalf("IsLoggedIn(existing) err = %v, want nil", err)
	}
	if loggedIn {
		t.Error("a token-less agent must report not logged in")
	}
}
