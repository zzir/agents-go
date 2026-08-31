package providers

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// ChatGPT OAuth configuration constants.
const (
	chatgptClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	chatgptAuthURL  = "https://auth.openai.com/oauth/authorize"
	chatgptTokenURL = "https://auth.openai.com/oauth/token"
	chatgptScope    = "openid profile email offline_access api.connectors.read api.connectors.invoke"
	// chatgptRedirectURI is fixed: OpenAI's Codex client only registers loopback
	// callbacks, and the token exchange must echo the same value the authorize
	// request used. Nothing on the server listens here — the user pastes the
	// redirected URL back to CompleteLogin (see decisions §5.41).
	chatgptRedirectURI = "http://localhost:1455/auth/callback"
	// ChatGPTBaseURL is the base URL for the ChatGPT Codex API.
	ChatGPTBaseURL = "https://chatgpt.com/backend-api/codex"
)

// chatgptLoginTTL bounds a pending login: the user must paste the callback URL
// within this window before the PKCE verifier is dropped. It matches the short
// life of the authorization code the callback carries.
const chatgptLoginTTL = 5 * time.Minute

// chatgptHTTPTimeout bounds every ChatGPT token endpoint call (exchange and
// refresh). Without it the default client waits forever, so a stalled OpenAX
// auth host would hang the session that triggered a token refresh.
const chatgptHTTPTimeout = 30 * time.Second

// ChatGPTOAuth manages the OAuth flow for ChatGPT subscription authentication.
type ChatGPTOAuth struct {
	providers *store.ProviderStore
	// settings routes token endpoint calls through the configured proxy_url.
	settings *settings.Reader

	mu      sync.Mutex
	pending map[string]*chatgptPending // keyed by state

	// refreshMu serializes token refreshes so two concurrent GetCredentials
	// calls don't both spend a single-use refresh token — the second rotation
	// would be rejected and log the user out.
	refreshMu sync.Mutex
}

type chatgptPending struct {
	providerID   string
	codeVerifier string
	// timer drops this entry after chatgptLoginTTL so an abandoned login (the
	// user never pastes the callback) cannot leak the verifier forever.
	timer *time.Timer
}

// NewChatGPTOAuth returns the OAuth manager over the provider rows it logs in
// and the settings its token calls honor (proxy_url).
func NewChatGPTOAuth(providers *store.ProviderStore, cfg *settings.Reader) *ChatGPTOAuth {
	if providers == nil || cfg == nil {
		panic("bridge: NewChatGPTOAuth needs the provider store and the setting reader")
	}
	return &ChatGPTOAuth{
		providers: providers,
		settings:  cfg,
		pending:   make(map[string]*chatgptPending),
	}
}

// httpClient returns a timed HTTP client for the token endpoint, routed through
// the configured proxy when one is set.
func (o *ChatGPTOAuth) httpClient(ctx context.Context) *http.Client {
	if pc := o.settings.ProxyClient(ctx); pc != nil {
		pc.Timeout = chatgptHTTPTimeout
		return pc
	}
	return &http.Client{Timeout: chatgptHTTPTimeout}
}

// ChatGPTLoginResult is returned by StartLogin with the authorize URL. The
// state is not exposed: it rides back inside the callback URL the user pastes
// to CompleteLogin, which is where the server reads it.
type ChatGPTLoginResult struct {
	AuthorizeURL string `json:"authorize_url"`
}

// StartLogin begins the ChatGPT OAuth PKCE flow for the given provider. It
// fails with store.ErrNotFound if the provider does not exist — otherwise the flow
// would run to completion and then silently lose the token on the final
// (no-op) update to a missing row.
func (o *ChatGPTOAuth) StartLogin(ctx context.Context, providerID string) (*ChatGPTLoginResult, error) {
	if providerID == "" {
		return nil, fmt.Errorf("provider_id is required")
	}
	pv, err := o.providers.Get(ctx, providerID)
	if err != nil {
		return nil, err
	}
	// A provider that does not authenticate by chatgpt_login can never use the
	// token, so completing the flow would only strand a credential in the
	// database with no UI path to revoke it — the disconnect button renders
	// for chatgpt_login providers only.
	if err := chatGPTLoginAvailable(pv); err != nil {
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

	params := url.Values{
		"response_type":              {"code"},
		"client_id":                  {chatgptClientID},
		"redirect_uri":               {chatgptRedirectURI},
		"scope":                      {chatgptScope},
		"code_challenge":             {challenge},
		"code_challenge_method":      {"S256"},
		"state":                      {state},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
		"originator":                 {"codex_cli_rs"},
	}
	authorizeURL := chatgptAuthURL + "?" + params.Encode()

	p := &chatgptPending{providerID: providerID, codeVerifier: verifier}
	o.mu.Lock()
	o.pending[state] = p
	p.timer = time.AfterFunc(chatgptLoginTTL, func() { o.cleanupPending(state) })
	o.mu.Unlock()

	return &ChatGPTLoginResult{AuthorizeURL: authorizeURL}, nil
}

// CompleteLogin finishes a login begun by StartLogin. It reads the
// authorization code and state from the callback URL the user pastes after
// authorizing, redeems the code for tokens server-side against the stored PKCE
// verifier, and saves them on the provider. This replaces the loopback listener
// the CLI-style flow used, so a remotely deployed server — where the browser's
// localhost is not the server's — can still be signed in (decisions §5.41).
func (o *ChatGPTOAuth) CompleteLogin(ctx context.Context, providerID, callback string) error {
	if providerID == "" {
		return fmt.Errorf("provider_id is required")
	}
	code, state, oauthErr, err := parseChatGPTCallback(callback)
	if err != nil {
		return err
	}
	if oauthErr != "" {
		return fmt.Errorf("%w: authorization failed: %s", ErrChatGPTCallbackInvalid, oauthErr)
	}
	if code == "" || state == "" {
		return fmt.Errorf("%w: no authorization code found in the URL", ErrChatGPTCallbackInvalid)
	}

	o.mu.Lock()
	p, ok := o.pending[state]
	o.mu.Unlock()
	if !ok {
		return ErrChatGPTLoginExpired
	}
	// The state is the only thing binding a callback to its verifier, so a URL
	// whose flow was started for another provider must not complete this one.
	if p.providerID != providerID {
		return fmt.Errorf("%w: this callback belongs to a different sign-in", ErrChatGPTCallbackInvalid)
	}

	tokens, err := exchangeCode(ctx, o.httpClient(ctx), code, p.codeVerifier, chatgptRedirectURI)
	if err != nil {
		return err
	}
	if err := o.saveTokens(ctx, providerID, tokens); err != nil {
		return err
	}
	// Redeem exactly once: only a stored token clears the pending entry, so a
	// transient exchange failure leaves the flow for the user to paste again
	// (until the TTL timer drops it).
	o.cleanupPending(state)
	return nil
}

// parseChatGPTCallback pulls the code, state, and any OAuth error out of the
// value the user pasted. It accepts a full redirect URL
// (http://localhost:1455/auth/callback?code=…&state=…), a bare query string, or
// one with a leading '?'.
func parseChatGPTCallback(raw string) (code, state, oauthErr string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", "", fmt.Errorf("%w: empty", ErrChatGPTCallbackInvalid)
	}
	var q url.Values
	if u, e := url.Parse(raw); e == nil && u.RawQuery != "" {
		q = u.Query()
	} else if vals, e := url.ParseQuery(strings.TrimPrefix(raw, "?")); e == nil {
		q = vals
	} else {
		return "", "", "", fmt.Errorf("%w: could not parse", ErrChatGPTCallbackInvalid)
	}
	return q.Get("code"), q.Get("state"), q.Get("error"), nil
}

// cleanupPending removes the pending flow for state and stops its expiry timer.
// Idempotent: CompleteLogin and the timer both call it, and whichever runs
// second finds nothing and is a no-op.
func (o *ChatGPTOAuth) cleanupPending(state string) {
	o.mu.Lock()
	p, ok := o.pending[state]
	if ok {
		delete(o.pending, state)
	}
	o.mu.Unlock()
	if ok && p.timer != nil {
		p.timer.Stop()
	}
}

// ChatGPTCredentials holds the access token and account ID for ChatGPT API calls.
type ChatGPTCredentials struct {
	AccessToken string
	AccountID   string
}

// GetCredentials returns the ChatGPT credentials stored on the provider.
func (o *ChatGPTOAuth) GetCredentials(ctx context.Context, providerID string) (*ChatGPTCredentials, error) {
	pv, err := o.providers.Get(ctx, providerID)
	if err != nil {
		return nil, fmt.Errorf("loading provider: %w", err)
	}
	if pv.ChatGPTToken == "" {
		return nil, fmt.Errorf("not logged in to ChatGPT for provider %s", providerID)
	}

	var tok chatgptTokens
	if err := json.Unmarshal([]byte(pv.ChatGPTToken), &tok); err != nil {
		return nil, fmt.Errorf("invalid stored tokens: %w", err)
	}

	if tokenExpiring(tok) {
		refreshed, err := o.refreshCredentials(ctx, providerID, tok)
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
func (o *ChatGPTOAuth) refreshCredentials(ctx context.Context, providerID string, tok chatgptTokens) (chatgptTokens, error) {
	o.refreshMu.Lock()
	defer o.refreshMu.Unlock()

	// Double-check under the lock: a racing refresh may have already rotated the
	// token while we waited.
	if pv, err := o.providers.Get(ctx, providerID); err == nil {
		var current chatgptTokens
		if json.Unmarshal([]byte(pv.ChatGPTToken), &current) == nil && current.AccessToken != "" {
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
	if err := o.saveTokens(ctx, providerID, refreshed); err != nil {
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

// Logout clears the provider stored ChatGPT token.
func (o *ChatGPTOAuth) Logout(ctx context.Context, providerID string) error {
	return o.providers.ClearChatGPTToken(ctx, providerID)
}

type chatgptTokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
}

// ErrChatGPTLoginUnavailable marks a login attempt this provider configuration
// can never use — a client error the handler maps to 400, not a server fault.
var ErrChatGPTLoginUnavailable = errors.New("chatgpt login unavailable")

// ErrChatGPTLoginExpired marks a completion whose pending flow is gone: the TTL
// elapsed, the state was already redeemed, or the server restarted since
// StartLogin. The user starts the sign-in again. The handler maps it to 400.
var ErrChatGPTLoginExpired = errors.New("chatgpt login expired — start the sign-in again")

// ErrChatGPTCallbackInvalid marks a pasted callback URL the server cannot act
// on: unparseable, missing the code/state, carrying an OAuth error from the
// provider, or bound to a different provider's flow. The handler maps it to 400.
var ErrChatGPTCallbackInvalid = errors.New("invalid callback URL")

// chatGPTLoginAvailable reports whether the provider backend offers
// chatgpt_login. It gates BOTH ends of the OAuth flow: StartLogin, and
// saveTokens — the row can change during the authorize window, and a token
// persisted onto a provider that cannot use it has no UI path to revoke it.
func chatGPTLoginAvailable(pv *store.Provider) error {
	def, err := DefFor(pv.Type)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrChatGPTLoginUnavailable, err)
	}
	if !slices.Contains(def.AuthModes, AuthModeChatGPTLogin) {
		return fmt.Errorf("%w: not offered by the %s provider — switch the provider type or use an API key", ErrChatGPTLoginUnavailable, def.Type)
	}
	// The ROW's own mode, not only the type's menu: logging in a provider that
	// authenticates by API key would strand a token nothing ever uses or shows
	// a disconnect button for.
	if pv.AuthMode != AuthModeChatGPTLogin {
		return fmt.Errorf("%w: this provider authenticates by API key — set auth_mode to %s first", ErrChatGPTLoginUnavailable, AuthModeChatGPTLogin)
	}
	return nil
}

func (o *ChatGPTOAuth) saveTokens(ctx context.Context, providerID string, tok *chatgptTokens) error {
	pv, err := o.providers.Get(ctx, providerID)
	if err != nil {
		return err
	}
	if err := chatGPTLoginAvailable(pv); err != nil {
		return err
	}
	data, _ := json.Marshal(tok)
	return o.providers.SaveChatGPTToken(ctx, providerID, string(data))
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
	tok.RefreshToken = cmp.Or(tok.RefreshToken, refresh)
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
