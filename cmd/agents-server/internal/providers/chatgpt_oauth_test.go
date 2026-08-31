package providers

import (
	"context"
	"errors"
	"testing"

	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// IsLoggedIn / StartLogin must distinguish a missing provider (ErrNotFound →
// the handler answers 404) from an existing one that simply isn't logged in, so
// the ChatGPT OAuth endpoints have consistent resource semantics.
func TestChatGPTOAuthMissingProvider(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
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
	pv := &store.Provider{Name: "a", OwnerID: store.NewID()}
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

// The pasted value may be a full redirect URL, a bare query string, or one with
// a leading '?'; all three must yield the same code/state, and an OAuth error
// param must surface separately.
func TestParseChatGPTCallback(t *testing.T) {
	cases := []struct {
		name, in, code, state, oerr string
		wantErr                     bool
	}{
		{"full url", "http://localhost:1455/auth/callback?code=ac_x&state=st1", "ac_x", "st1", "", false},
		{"bare query", "code=ac_y&state=st2", "ac_y", "st2", "", false},
		{"leading question mark", "?code=ac_z&state=st3", "ac_z", "st3", "", false},
		{"surrounding whitespace", "  http://localhost:1455/auth/callback?code=ac_w&state=st4\n", "ac_w", "st4", "", false},
		{"oauth error", "http://localhost:1455/auth/callback?error=access_denied&state=st5", "", "st5", "access_denied", false},
		{"empty", "   ", "", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, state, oerr, err := parseChatGPTCallback(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("parseChatGPTCallback(%q) err = nil, want error", c.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseChatGPTCallback(%q) err = %v", c.in, err)
			}
			if code != c.code || state != c.state || oerr != c.oerr {
				t.Fatalf("parseChatGPTCallback(%q) = (%q,%q,%q), want (%q,%q,%q)",
					c.in, code, state, oerr, c.code, c.state, c.oerr)
			}
		})
	}
}

// CompleteLogin's guard branches — before any token exchange — must return the
// typed errors the handler maps to 400: an unknown state is expired, a missing
// code or a provider mismatch is an invalid callback.
func TestCompleteLoginGuards(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	o := NewChatGPTOAuth(store.NewProviderStore(db), settings.NewReader(store.NewSettingStore(db)))

	if err := o.CompleteLogin(ctx, "pid", "http://localhost:1455/auth/callback?code=ac_x&state=unknown"); !errors.Is(err, ErrChatGPTLoginExpired) {
		t.Fatalf("unknown state err = %v, want ErrChatGPTLoginExpired", err)
	}
	if err := o.CompleteLogin(ctx, "pid", "http://localhost:1455/auth/callback?state=unknown"); !errors.Is(err, ErrChatGPTCallbackInvalid) {
		t.Fatalf("missing code err = %v, want ErrChatGPTCallbackInvalid", err)
	}

	// A pending flow bound to provider A must reject a completion aimed at B.
	o.mu.Lock()
	o.pending["st"] = &chatgptPending{providerID: "A", codeVerifier: "v"}
	o.mu.Unlock()
	if err := o.CompleteLogin(ctx, "B", "http://localhost:1455/auth/callback?code=ac_x&state=st"); !errors.Is(err, ErrChatGPTCallbackInvalid) {
		t.Fatalf("provider mismatch err = %v, want ErrChatGPTCallbackInvalid", err)
	}
}
