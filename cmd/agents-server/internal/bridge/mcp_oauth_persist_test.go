package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"golang.org/x/oauth2"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// newOAuthTestStore returns a store with one MCP server row whose saved grant
// is payload (marshalled), plus the row id.
func newOAuthTestStore(t *testing.T, payload *tokenPayload) (*store.McpServerStore, string) {
	t.Helper()
	s := store.NewMcpServerStore(testdb.New(t))
	cfg := &store.McpServerConfig{
		ID: store.NewID(), Name: "srv", TransportType: "streamable_http", Enabled: true,
	}
	if err := s.Create(context.Background(), cfg); err != nil {
		t.Fatalf("create config: %v", err)
	}
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		if err := s.SaveOAuthToken(context.Background(), cfg.ID, string(b)); err != nil {
			t.Fatalf("seed token: %v", err)
		}
	}
	return s, cfg.ID
}

// savedPayload reads back the persisted grant for id.
func savedPayload(t *testing.T, s *store.McpServerStore, id string) tokenPayload {
	t.Helper()
	cfg, err := s.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	var p tokenPayload
	if err := json.Unmarshal([]byte(cfg.OAuthToken), &p); err != nil {
		t.Fatalf("unmarshal saved grant %q: %v", cfg.OAuthToken, err)
	}
	return p
}

// staticSeq returns each token once, then keeps returning the last one.
type staticSeq struct {
	toks []*oauth2.Token
	i    int
}

func (s *staticSeq) Token() (*oauth2.Token, error) {
	tok := s.toks[min(s.i, len(s.toks)-1)]
	s.i++
	return tok, nil
}

// A refreshed token — including a ROTATED refresh token — must be written
// back to the store exactly when it changes, not on every Token() call.
func TestPersistingTokenSourceRepersistsOnChange(t *testing.T) {
	s, id := newOAuthTestStore(t, &tokenPayload{AccessToken: "at-1", RefreshToken: "rt-1"})
	ocfg := &oauth2.Config{ClientID: "cid", ClientSecret: "sec",
		Endpoint: oauth2.Endpoint{TokenURL: "https://as.example/token", AuthStyle: oauth2.AuthStyleInHeader}}

	tokA := &oauth2.Token{AccessToken: "at-1", RefreshToken: "rt-1"}
	tokB := &oauth2.Token{AccessToken: "at-2", RefreshToken: "rt-2"}
	src := newPersistingSource(t.Context(), &staticSeq{toks: []*oauth2.Token{tokA, tokA, tokB}}, ocfg, id, s, tokA)

	for range 2 { // unchanged token: nothing re-written
		if _, err := src.Token(); err != nil {
			t.Fatalf("token: %v", err)
		}
	}
	if got := savedPayload(t, s, id); got.AccessToken != "at-1" || got.TokenURL != "" {
		t.Fatalf("unchanged token was re-persisted: %+v", got)
	}

	if _, err := src.Token(); err != nil { // refresh happened: full grant written
		t.Fatalf("token: %v", err)
	}
	got := savedPayload(t, s, id)
	if got.AccessToken != "at-2" || got.RefreshToken != "rt-2" {
		t.Fatalf("rotated grant not persisted: %+v", got)
	}
	if got.TokenURL != "https://as.example/token" || got.ClientID != "cid" || got.ClientSecret != "sec" ||
		got.AuthStyle != int(oauth2.AuthStyleInHeader) {
		t.Fatalf("refresh context not persisted alongside the token: %+v", got)
	}
}

// A full persisted grant with an EXPIRED access token must yield a source
// that refreshes against the token endpoint — no interactive re-auth — and
// re-persists the refreshed (rotated) grant. This is the restart path.
func TestRestoredTokenSourceRefreshesExpiredGrant(t *testing.T) {
	var sawGrant, sawRefreshToken atomic.Value
	as := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		sawGrant.Store(r.Form.Get("grant_type"))
		sawRefreshToken.Store(r.Form.Get("refresh_token"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at-new","refresh_token":"rt-new","token_type":"Bearer","expires_in":3600}`))
	}))
	defer as.Close()

	seed := &tokenPayload{
		AccessToken: "at-old", RefreshToken: "rt-old", TokenType: "Bearer",
		Expiry:   time.Now().Add(-time.Hour), // expired
		TokenURL: as.URL, ClientID: "cid", Scopes: []string{"a", "b"},
	}
	s, id := newOAuthTestStore(t, seed)
	b, _ := json.Marshal(seed)

	ts := restoredTokenSource(t.Context(), id, string(b), s, &http.Client{Timeout: 5 * time.Second})
	if ts == nil {
		t.Fatal("full grant produced no token source")
	}
	tok, err := ts.Token()
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if tok.AccessToken != "at-new" {
		t.Fatalf("access token = %q, want at-new", tok.AccessToken)
	}
	if sawGrant.Load() != "refresh_token" || sawRefreshToken.Load() != "rt-old" {
		t.Fatalf("token endpoint saw grant_type=%v refresh_token=%v", sawGrant.Load(), sawRefreshToken.Load())
	}
	got := savedPayload(t, s, id)
	if got.AccessToken != "at-new" || got.RefreshToken != "rt-new" {
		t.Fatalf("refreshed grant not re-persisted: %+v", got)
	}
	if got.TokenURL != as.URL || got.ClientID != "cid" {
		t.Fatalf("refresh context lost on re-persist: %+v", got)
	}
}

// Legacy payloads (written before refresh support) and unusable grants must
// degrade exactly as before: valid access token → static source; anything
// else → nil (interactive flow).
func TestRestoredTokenSourceLegacyAndInvalid(t *testing.T) {
	s, id := newOAuthTestStore(t, nil)

	legacyValid, _ := json.Marshal(tokenPayload{
		AccessToken: "at", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour),
	})
	ts := restoredTokenSource(t.Context(), id, string(legacyValid), s, http.DefaultClient)
	if ts == nil {
		t.Fatal("legacy valid grant should yield a static source")
	}
	if tok, err := ts.Token(); err != nil || tok.AccessToken != "at" {
		t.Fatalf("static token = %v, %v", tok, err)
	}

	legacyExpired, _ := json.Marshal(tokenPayload{
		AccessToken: "at", Expiry: time.Now().Add(-time.Hour),
	})
	for name, saved := range map[string]string{
		"legacy expired": string(legacyExpired),
		"empty":          "",
		"garbage":        "{not json",
	} {
		if got := restoredTokenSource(t.Context(), id, saved, s, http.DefaultClient); got != nil {
			t.Errorf("%s: want nil source, got %T", name, got)
		}
	}
}

// Only the interactive phase may park the fetcher for a popup. In the silent
// phase (saved grant rejected during a token-based connect) and once
// established (a 401 the refresh token couldn't fix, long after connect), an
// Authorize must fail fast — nobody is watching for a popup URL, and parking
// would hang the request until its context expires.
func TestConnectFetcherParksOnlyWhileInteractive(t *testing.T) {
	urlCh := make(chan string, 1)
	codeCh := make(chan *auth.AuthorizationResult, 1)
	fetcher, phase := newConnectFetcher("srv", urlCh, codeCh)

	// failsFast asserts an Authorize in the current phase errors immediately
	// without publishing a popup URL.
	failsFast := func(when string) {
		t.Helper()
		done := make(chan error, 1)
		go func() {
			_, err := fetcher(context.Background(), &auth.AuthorizationArgs{URL: "https://as/authorize?state=x"})
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Fatalf("%s: Authorize should fail", when)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: Authorize parked instead of failing fast", when)
		}
		select {
		case u := <-urlCh:
			t.Fatalf("%s: published a popup URL: %q", when, u)
		default:
		}
	}

	failsFast("silent phase") // initial phase: saved-grant connect in progress

	// Interactive: the fetcher publishes the URL and returns the delivered code.
	phase.Store(oauthPhaseInteractive)
	codeCh <- &auth.AuthorizationResult{Code: "c1"}
	res, err := fetcher(context.Background(), &auth.AuthorizationArgs{URL: "https://as/authorize?state=s"})
	if err != nil || res.Code != "c1" {
		t.Fatalf("interactive fetch = %v, %v", res, err)
	}
	if got := <-urlCh; got == "" {
		t.Fatal("authorize URL not published")
	}

	phase.Store(oauthPhaseEstablished)
	failsFast("established phase")
}
