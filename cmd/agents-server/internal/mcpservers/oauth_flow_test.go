package mcpservers

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// fakeAS is a minimal OAuth 2.1 authorization server: metadata discovery,
// dynamic client registration, an authorize endpoint that redirects straight
// back with a code (no consent page), and a token endpoint with PKCE S256.
type fakeAS struct {
	srv *httptest.Server
	mu  sync.Mutex
	// clients maps client_id -> registered redirect URIs.
	clients map[string][]string
	codes   map[string]string // code -> code_challenge
	// accessToken is what /token hands out; the resource server accepts it.
	accessToken string
}

func newFakeAS(t *testing.T) *fakeAS {
	t.Helper()
	as := &fakeAS{clients: map[string][]string{}, codes: map[string]string{}, accessToken: "at-" + rand.Text()}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&oauthex.AuthServerMeta{
			Issuer:                                     as.srv.URL,
			AuthorizationEndpoint:                      as.srv.URL + "/authorize",
			TokenEndpoint:                              as.srv.URL + "/token",
			RegistrationEndpoint:                       as.srv.URL + "/register",
			ResponseTypesSupported:                     []string{"code"},
			CodeChallengeMethodsSupported:              []string{"S256"},
			TokenEndpointAuthMethodsSupported:          []string{"client_secret_post", "none"},
			GrantTypesSupported:                        []string{"authorization_code", "refresh_token"},
			AuthorizationResponseIssParameterSupported: true,
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		var meta oauthex.ClientRegistrationMetadata
		if err := json.NewDecoder(r.Body).Decode(&meta); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		id := "client-" + rand.Text()
		as.mu.Lock()
		as.clients[id] = meta.RedirectURIs
		as.mu.Unlock()
		meta.TokenEndpointAuthMethod = "none"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(&oauthex.ClientRegistrationResponse{ClientID: id, ClientRegistrationMetadata: meta})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		as.mu.Lock()
		uris, ok := as.clients[q.Get("client_id")]
		as.mu.Unlock()
		if !ok {
			http.Error(w, "unknown client_id", http.StatusBadRequest)
			return
		}
		redirect := q.Get("redirect_uri")
		found := false
		for _, u := range uris {
			if u == redirect {
				found = true
			}
		}
		if !found {
			http.Error(w, "invalid redirect_uri "+redirect, http.StatusBadRequest)
			return
		}
		code := "code-" + rand.Text()
		as.mu.Lock()
		as.codes[code] = q.Get("code_challenge")
		as.mu.Unlock()
		http.Redirect(w, r, fmt.Sprintf("%s?code=%s&state=%s&iss=%s", redirect, code, q.Get("state"), url.QueryEscape(as.srv.URL)), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if r.Form.Get("grant_type") != "authorization_code" {
			http.Error(w, "unsupported grant_type", http.StatusBadRequest)
			return
		}
		as.mu.Lock()
		challenge, ok := as.codes[r.Form.Get("code")]
		delete(as.codes, r.Form.Get("code"))
		as.mu.Unlock()
		if !ok {
			http.Error(w, "unknown code", http.StatusBadRequest)
			return
		}
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
			http.Error(w, "pkce mismatch", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  as.accessToken,
			"token_type":    "Bearer",
			"expires_in":    3600,
			"refresh_token": "rt-" + rand.Text(),
		})
	})
	as.srv = httptest.NewServer(mux)
	t.Cleanup(as.srv.Close)
	return as
}

// newProtectedMCP serves a one-tool MCP server at /mcp behind a bearer check,
// advertising PRM for the discovery chain. acceptToken decides which tokens the
// resource server honors; nil accepts exactly the AS's issued access token. A
// func that never accepts models a real mismatch — the authorization completes
// but the issued token's audience/scope is not what this resource wants — so
// every request 401s even after a successful authorize.
func newProtectedMCP(t *testing.T, as *fakeAS, wrap func(http.Handler) http.Handler, acceptToken func(string) bool) *httptest.Server {
	t.Helper()
	if acceptToken == nil {
		acceptToken = func(tok string) bool { return tok == as.accessToken }
	}
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "protected", Version: "1.0"}, nil)
	mcpsdk.AddTool(srv, &mcpsdk.Tool{Name: "ping", Description: "answer"},
		func(context.Context, *mcpsdk.CallToolRequest, struct{}) (*mcpsdk.CallToolResult, any, error) {
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "pong"}}}, nil, nil
		})
	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return srv },
		&mcpsdk.StreamableHTTPOptions{JSONResponse: true})

	mux := http.NewServeMux()
	rs := httptest.NewUnstartedServer(mux)
	rs.Start()
	t.Cleanup(rs.Close)
	mux.Handle("/.well-known/oauth-protected-resource/mcp", auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
		Resource:             rs.URL + "/mcp",
		AuthorizationServers: []string{as.srv.URL},
	}))
	verifier := func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		if !acceptToken(token) {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{Expiration: time.Now().Add(time.Hour)}, nil
	}
	var inner http.Handler = handler
	if wrap != nil {
		inner = wrap(handler)
	}
	mux.Handle("/mcp", auth.RequireBearerToken(verifier, &auth.RequireBearerTokenOptions{
		ResourceMetadataURL: rs.URL + "/.well-known/oauth-protected-resource/mcp",
	})(inner))
	return rs
}

// legacyServer rejects server/discover the way a pre-2026-07-28 server does
// (a 400 JSON-RPC error), forcing the legacy initialize handshake and the
// standalone SSE GET that follows it.
func legacyServer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			if bytes.Contains(body, []byte(`"server/discover"`)) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32000,"message":"Bad Request: No valid session ID provided"},"id":null}`))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// The interactive OAuth flow end to end: Connect parks on the popup, the
// callback delivers the code, and the connection comes up — the coordinator
// must then report the server connected, not authorizing forever.
func TestConnectWithOAuthInteractiveFlowCompletes(t *testing.T) {
	for _, tc := range []struct {
		name string
		wrap func(http.Handler) http.Handler
	}{
		{"modern", nil},
		{"legacy", legacyServer},
	} {
		t.Run(tc.name, func(t *testing.T) { testInteractiveFlow(t, tc.wrap) })
	}
}

func testInteractiveFlow(t *testing.T, wrap func(http.Handler) http.Handler) {
	t.Helper()
	as := newFakeAS(t)
	rs := newProtectedMCP(t, as, wrap, nil)
	c, mgr, st, cfg := startInteractiveConnect(t, rs)

	deadline := time.Now().Add(15 * time.Second)
	for !mgr.IsConnected(cfg.ID) {
		if time.Now().After(deadline) {
			t.Fatalf("never connected: authorizing=%v connecting=%v", c.IsAuthorizing(cfg.ID), mgr.IsConnecting(cfg.ID))
		}
		time.Sleep(50 * time.Millisecond)
	}
	for c.IsAuthorizing(cfg.ID) {
		if time.Now().After(deadline) {
			t.Fatal("still reported authorizing after connect")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := savedPayload(t, st, cfg.ID); got.AccessToken != as.accessToken {
		t.Fatalf("persisted access token = %q, want %q", got.AccessToken, as.accessToken)
	}
}

// A resource server that rejects even the freshly-issued token (audience/scope
// mismatch) drives the SDK to Authorize a SECOND time inside one connect. The
// interactive park is single-shot — there is no second popup to service — so
// the attempt must fail fast and leave the authorizing state, not hang until
// the 5-minute pending timeout. This is the reported "stuck Authorizing…".
func TestConnectWithOAuthTokenRejectedDoesNotHang(t *testing.T) {
	as := newFakeAS(t)
	rs := newProtectedMCP(t, as, nil, func(string) bool { return false })
	c, mgr, _, cfg := startInteractiveConnect(t, rs)

	// After the code is delivered the attempt must resolve — connected (it
	// won't, the token is rejected) or terminal — within well under the
	// 5-minute pending timeout. A hang keeps IsAuthorizing true forever.
	deadline := time.Now().Add(30 * time.Second)
	for c.IsAuthorizing(cfg.ID) {
		if time.Now().After(deadline) {
			t.Fatal("still authorizing 30s after the callback — the second Authorize re-parked and hung")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if mgr.IsConnected(cfg.ID) {
		t.Fatal("must not report connected when the token is rejected")
	}
}

// startInteractiveConnect runs ConnectWithOAuth for an interactive login,
// drives the "browser" through the authorize redirect, and delivers the code
// via HandleCallback — returning once the code is in flight. The caller asserts
// how the attempt then resolves.
func startInteractiveConnect(t *testing.T, rs *httptest.Server) (*OAuthCoordinator, *Manager, *store.McpServerStore, *store.McpServerConfig) {
	t.Helper()
	db := testdb.New(t)
	st := store.NewMcpServerStore(db)
	hc := store.HTTPMcpConfig{Endpoint: rs.URL + "/mcp", AuthMode: "oauth"}
	raw, _ := json.Marshal(hc)
	cfg := &store.McpServerConfig{ID: store.NewID(), Name: "srv", Enabled: true, Config: raw}
	if err := st.Create(context.Background(), cfg); err != nil {
		t.Fatalf("create: %v", err)
	}
	mgr := NewManager(context.Background(), nil)
	t.Cleanup(mgr.CloseAll)
	c := NewOAuthCoordinator(st)

	// The request context ends when the handler returns — mirror that.
	reqCtx, cancelReq := context.WithCancel(context.Background())
	res, err := c.ConnectWithOAuth(reqCtx, mgr, cfg, &hc, "http://127.0.0.1:1/mcp-servers/oauth/callback")
	cancelReq()
	if err != nil {
		t.Fatalf("ConnectWithOAuth: %v", err)
	}
	if res.Connected || res.AuthorizeURL == "" {
		t.Fatalf("expected authorization_required, got %+v", res)
	}
	if !c.IsAuthorizing(cfg.ID) {
		t.Fatal("flow should be in progress")
	}

	// The "browser": visit the authorize URL, capture the redirect back.
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noRedirect.Get(res.AuthorizeURL)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	resp.Body.Close()
	loc, err := resp.Location()
	if err != nil {
		t.Fatalf("authorize redirect (status %d): %v", resp.StatusCode, err)
	}
	if err := c.HandleCallback(loc.Query().Get("state"), loc.Query().Get("code"), loc.Query().Get("iss")); err != nil {
		t.Fatalf("HandleCallback: %v", err)
	}
	return c, mgr, st, cfg
}
