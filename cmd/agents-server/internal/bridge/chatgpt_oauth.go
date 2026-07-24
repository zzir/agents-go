package bridge

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// ChatGPT OAuth configuration constants.
const (
	chatgptClientID    = "app_EMoamEEZ73f0CkXaXp7hrann"
	chatgptAuthURL     = "https://auth.openai.com/oauth/authorize"
	chatgptTokenURL    = "https://auth.openai.com/oauth/token"
	chatgptScope       = "openid profile email offline_access api.connectors.read api.connectors.invoke"
	chatgptRedirectURI = "http://localhost:1455/auth/callback"
	chatgptFallbackURI = "http://localhost:1457/auth/callback"
	// ChatGPTBaseURL is the base URL for the ChatGPT Codex API.
	ChatGPTBaseURL = "https://chatgpt.com/backend-api/codex"
)

// chatgptHTTPTimeout bounds every ChatGPT token endpoint call (exchange and
// refresh). Without it the default client waits forever, so a stalled OpenAX
// auth host would hang the session that triggered a token refresh.
const chatgptHTTPTimeout = 30 * time.Second

// ChatGPTOAuth manages the OAuth flow for ChatGPT subscription authentication.
type ChatGPTOAuth struct {
	agents *store.AgentConfigStore
	// settings, when wired via UseSettings, routes token endpoint calls through
	// the configured proxy. Optional: nil means a direct (still timed) client.
	settings *store.SettingStore

	mu      sync.Mutex
	pending map[string]*chatgptPending // keyed by state

	// refreshMu serializes token refreshes so two concurrent GetCredentials
	// calls don't both spend a single-use refresh token — the second rotation
	// would be rejected and log the user out.
	refreshMu sync.Mutex
}

type chatgptPending struct {
	agentConfigID string
	codeVerifier  string
	redirectURI   string
	cancel        context.CancelFunc
}

// NewChatGPTOAuth creates a new ChatGPT OAuth manager.
func NewChatGPTOAuth(agents *store.AgentConfigStore) *ChatGPTOAuth {
	return &ChatGPTOAuth{
		agents:  agents,
		pending: make(map[string]*chatgptPending),
	}
}

// UseSettings wires a settings store so token endpoint calls honor the
// configured proxy_url. Optional — call once at construction. NOTE for merge:
// cmd/root.go's NewChatGPTOAuth call site should follow with
// chatgptOAuth.UseSettings(settingStore) to enable proxying of ChatGPT auth
// traffic; without it token calls go direct (still timed).
func (o *ChatGPTOAuth) UseSettings(settings *store.SettingStore) {
	o.settings = settings
}

// httpClient returns a timed HTTP client for the token endpoint, routed through
// the configured proxy when settings are wired.
func (o *ChatGPTOAuth) httpClient(ctx context.Context) *http.Client {
	if o.settings != nil {
		if pc := ProxyHTTPClient(ctx, o.settings); pc != nil {
			pc.Timeout = chatgptHTTPTimeout
			return pc
		}
	}
	return &http.Client{Timeout: chatgptHTTPTimeout}
}

// ChatGPTLoginResult is returned by StartLogin with the authorize URL.
type ChatGPTLoginResult struct {
	AuthorizeURL string `json:"authorize_url"`
	State        string `json:"state"`
}

// StartLogin begins the ChatGPT OAuth PKCE flow for the given agent. It fails
// with store.ErrNotFound if the agent does not exist — otherwise the flow
// would run to completion and then silently lose the token on the final
// (no-op) update to a missing row.
func (o *ChatGPTOAuth) StartLogin(ctx context.Context, agentConfigID string) (*ChatGPTLoginResult, error) {
	if agentConfigID == "" {
		return nil, fmt.Errorf("agent_config_id is required")
	}
	if _, err := o.agents.Get(ctx, agentConfigID); err != nil {
		return nil, err
	}

	state, err := randomString(32)
	if err != nil {
		return nil, fmt.Errorf("generating state: %w", err)
	}
	verifier, err := randomString(64)
	if err != nil {
		return nil, fmt.Errorf("generating verifier: %w", err)
	}
	challenge := pkceChallenge(verifier)

	redirectURI, listener, err := listenCallback()
	if err != nil {
		return nil, fmt.Errorf("starting callback listener: %w", err)
	}

	params := url.Values{
		"response_type":              {"code"},
		"client_id":                  {chatgptClientID},
		"redirect_uri":               {redirectURI},
		"scope":                      {chatgptScope},
		"code_challenge":             {challenge},
		"code_challenge_method":      {"S256"},
		"state":                      {state},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
		"originator":                 {"codex_cli_rs"},
	}
	authorizeURL := chatgptAuthURL + "?" + params.Encode()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)

	p := &chatgptPending{
		agentConfigID: agentConfigID,
		codeVerifier:  verifier,
		redirectURI:   redirectURI,
		cancel:        cancel,
	}

	o.mu.Lock()
	o.pending[state] = p
	o.mu.Unlock()

	go o.serveCallback(ctx, listener, state)

	return &ChatGPTLoginResult{
		AuthorizeURL: authorizeURL,
		State:        state,
	}, nil
}

func listenCallback() (string, net.Listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:1455")
	if err == nil {
		return chatgptRedirectURI, ln, nil
	}
	ln, err = net.Listen("tcp", "127.0.0.1:1457")
	if err == nil {
		return chatgptFallbackURI, ln, nil
	}
	return "", nil, fmt.Errorf("cannot listen on port 1455 or 1457: %w", err)
}

func (o *ChatGPTOAuth) serveCallback(ctx context.Context, ln net.Listener, state string) {
	mux := http.NewServeMux()
	srv := &http.Server{Handler: mux}

	writeHTML := func(w http.ResponseWriter, status, errMsg string) {
		_, _ = fmt.Fprint(w, callbackHTML(status, errMsg))
	}
	shutdown := func() { go func() { _ = srv.Shutdown(context.Background()) }() }

	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		gotState := r.URL.Query().Get("state")
		errMsg := r.URL.Query().Get("error")

		w.Header().Set("Content-Type", "text/html; charset=utf-8")

		if errMsg != "" {
			writeHTML(w, "error", errMsg)
			o.cleanupPending(state)
			shutdown()
			return
		}
		if gotState != state || code == "" {
			writeHTML(w, "error", "invalid state or missing code")
			o.cleanupPending(state)
			shutdown()
			return
		}

		o.mu.Lock()
		p, ok := o.pending[state]
		o.mu.Unlock()
		if !ok {
			writeHTML(w, "error", "expired")
			shutdown()
			return
		}

		// Bound the token exchange by the flow context (5-minute timeout) and a
		// timed client, so a stalled auth host can't wedge the callback.
		tokens, err := exchangeCode(ctx, o.httpClient(ctx), code, p.codeVerifier, p.redirectURI)
		if err != nil {
			writeHTML(w, "error", err.Error())
			o.cleanupPending(state)
			shutdown()
			return
		}

		if err := o.saveTokens(r.Context(), p.agentConfigID, tokens); err != nil {
			writeHTML(w, "error", err.Error())
			o.cleanupPending(state)
			shutdown()
			return
		}

		writeHTML(w, "success", "")
		o.cleanupPending(state)
		shutdown()
	})

	go func() {
		<-ctx.Done()
		// The flow ended (success, error, or the 5-minute timeout): drop any
		// pending entry so an abandoned login can't leak it forever.
		o.cleanupPending(state)
		_ = srv.Shutdown(context.Background())
	}()

	_ = srv.Serve(ln)
}

// cleanupPending removes the pending flow for state and cancels its context.
// Idempotent: the callback handler and the flow's context watcher both call it,
// and whichever runs second is a no-op.
func (o *ChatGPTOAuth) cleanupPending(state string) {
	o.mu.Lock()
	p, ok := o.pending[state]
	if ok {
		delete(o.pending, state)
	}
	o.mu.Unlock()
	if ok {
		p.cancel()
	}
}

// ChatGPTCredentials holds the access token and account ID for ChatGPT API calls.
type ChatGPTCredentials struct {
	AccessToken string
	AccountID   string
}

// GetCredentials returns the ChatGPT credentials for the given agent config.
func (o *ChatGPTOAuth) GetCredentials(ctx context.Context, agentConfigID string) (*ChatGPTCredentials, error) {
	ac, err := o.agents.Get(ctx, agentConfigID)
	if err != nil {
		return nil, fmt.Errorf("loading agent config: %w", err)
	}
	if ac.ChatGPTToken == "" {
		return nil, fmt.Errorf("not logged in to ChatGPT for agent %s", agentConfigID)
	}

	var tok chatgptTokens
	if err := json.Unmarshal([]byte(ac.ChatGPTToken), &tok); err != nil {
		return nil, fmt.Errorf("invalid stored tokens: %w", err)
	}

	if tokenExpiring(tok) {
		refreshed, err := o.refreshCredentials(ctx, agentConfigID, tok)
		if err != nil {
			return nil, err
		}
		tok = refreshed
	}

	accountID := decodeAccountID(tok.AccessToken)
	return &ChatGPTCredentials{
		AccessToken: tok.AccessToken,
		AccountID:   accountID,
	}, nil
}

// tokenExpiring reports whether tok is within a minute of expiry (or already
// past it), the threshold at which GetCredentials refreshes.
func tokenExpiring(tok chatgptTokens) bool {
	return tok.ExpiresAt > 0 && time.Now().Unix() > tok.ExpiresAt-60
}

// refreshCredentials rotates the stored token under refreshMu so concurrent
// callers don't both spend the single-use refresh token. After taking the
// lock it re-reads the stored token: if another goroutine already refreshed it,
// that result is reused instead of spending the refresh token a second time.
func (o *ChatGPTOAuth) refreshCredentials(ctx context.Context, agentConfigID string, tok chatgptTokens) (chatgptTokens, error) {
	o.refreshMu.Lock()
	defer o.refreshMu.Unlock()

	// Double-check under the lock: a racing refresh may have already rotated the
	// token while we waited.
	if ac, err := o.agents.Get(ctx, agentConfigID); err == nil {
		var current chatgptTokens
		if json.Unmarshal([]byte(ac.ChatGPTToken), &current) == nil && current.AccessToken != "" {
			if !tokenExpiring(current) {
				return current, nil
			}
			tok = current
		}
	}

	refreshed, err := refreshToken(ctx, o.httpClient(ctx), tok.RefreshToken)
	if err != nil {
		return chatgptTokens{}, fmt.Errorf("token refresh failed: %w", err)
	}
	if err := o.saveTokens(ctx, agentConfigID, refreshed); err != nil {
		return chatgptTokens{}, err
	}
	return *refreshed, nil
}

func decodeAccountID(jwt string) string {
	parts := strings.SplitN(jwt, ".", 3)
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Auth struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return claims.Auth.AccountID
}

// IsLoggedIn returns whether the given agent has a ChatGPT token stored.
// IsLoggedIn reports whether the agent has a stored ChatGPT token. It returns
// the store error (e.g. store.ErrNotFound) so callers can distinguish "agent
// does not exist" from "exists but not logged in" — matching the 404 the
// login/logout endpoints give for a missing agent.
func (o *ChatGPTOAuth) IsLoggedIn(ctx context.Context, agentConfigID string) (bool, error) {
	if agentConfigID == "" {
		return false, fmt.Errorf("agent_config_id is required")
	}
	ac, err := o.agents.Get(ctx, agentConfigID)
	if err != nil {
		return false, err
	}
	return ac.ChatGPTToken != "", nil
}

// Logout clears the ChatGPT token for the given agent.
func (o *ChatGPTOAuth) Logout(ctx context.Context, agentConfigID string) error {
	return o.agents.ClearChatGPTToken(ctx, agentConfigID)
}

type chatgptTokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
}

func (o *ChatGPTOAuth) saveTokens(ctx context.Context, agentConfigID string, tok *chatgptTokens) error {
	data, _ := json.Marshal(tok)
	return o.agents.SaveChatGPTToken(ctx, agentConfigID, string(data))
}

func exchangeCode(ctx context.Context, client *http.Client, code, verifier, redirectURI string) (*chatgptTokens, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {chatgptClientID},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatgptTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("token exchange request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token exchange request: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Error        string `json:"error"`
		ErrorDesc    string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding token response: %w", err)
	}
	if result.Error != "" {
		return nil, fmt.Errorf("%s: %s", result.Error, result.ErrorDesc)
	}

	tok := &chatgptTokens{
		AccessToken:  result.AccessToken,
		RefreshToken: result.RefreshToken,
		IDToken:      result.IDToken,
	}
	if result.ExpiresIn > 0 {
		tok.ExpiresAt = time.Now().Unix() + result.ExpiresIn
	}
	return tok, nil
}

func refreshToken(ctx context.Context, client *http.Client, refresh string) (*chatgptTokens, error) {
	body, _ := json.Marshal(map[string]string{
		"client_id":     chatgptClientID,
		"grant_type":    "refresh_token",
		"refresh_token": refresh,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatgptTokenURL, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Error        string `json:"error"`
		ErrorDesc    string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if result.Error != "" {
		return nil, fmt.Errorf("%s: %s", result.Error, result.ErrorDesc)
	}

	tok := &chatgptTokens{
		AccessToken:  result.AccessToken,
		IDToken:      result.IDToken,
		RefreshToken: result.RefreshToken,
	}
	if tok.RefreshToken == "" {
		tok.RefreshToken = refresh
	}
	if result.ExpiresIn > 0 {
		tok.ExpiresAt = time.Now().Unix() + result.ExpiresIn
	}
	return tok, nil
}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func pkceChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func callbackHTML(status, errMsg string) string {
	msg := "Authorization successful. You can close this window."
	if status != "success" {
		msg = "Authorization failed: " + errMsg
	}
	return `<!DOCTYPE html><html><body><p>` + msg +
		`</p><script>
if (window.opener) {
  window.opener.postMessage({type:'chatgpt-oauth-done',status:'` + status + `'}, '*');
}
setTimeout(function(){ window.close(); }, 1500);
</script></body></html>`
}
