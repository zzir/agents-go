package bridge

import (
	"context"
	"errors"
	"testing"

	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// IsLoggedIn / StartLogin must distinguish a missing provider (ErrNotFound →
// the handler answers 404) from an existing one that simply isn't logged in, so
// the ChatGPT OAuth endpoints have consistent resource semantics.
func TestChatGPTOAuthMissingProvider(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	providers := store.NewProviderStore(db)
	o := NewChatGPTOAuth(providers, settings.NewReader(store.NewSettingStore(db)))

	// Missing provider -> ErrNotFound, not a folded logged_in:false.
	if _, err := o.IsLoggedIn(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("IsLoggedIn(missing) err = %v, want ErrNotFound", err)
	}
	if _, err := o.StartLogin(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("StartLogin(missing) err = %v, want ErrNotFound", err)
	}

	// Existing provider with no token -> (false, nil), a real "not logged in".
	pv := &store.Provider{Name: "a"}
	if err := providers.Create(ctx, pv); err != nil {
		t.Fatalf("create: %v", err)
	}
	loggedIn, err := o.IsLoggedIn(ctx, pv.ID)
	if err != nil {
		t.Fatalf("IsLoggedIn(existing) err = %v, want nil", err)
	}
	if loggedIn {
		t.Error("a token-less provider must report not logged in")
	}
}
