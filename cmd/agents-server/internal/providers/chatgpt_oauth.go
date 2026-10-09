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
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// ChatGPT OAuth configuration constants.
const (
	chatgptClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	chatgptAuthURL  = "https://auth.openai.com/oauth/authorize"
	chatgptTokenURL = "https://auth.openai.com/oauth/token"
	chatgptScope    = "openid profile email offline_access api.connectors.read api.connectors.invoke"
	// chatgptRedirectURI is what the token exchange must echo; nothing listens
	// on it — decisions §5.41.
	chatgptRedirectURI = "http://localhost:1455/auth/callback"
	// ChatGPTBaseURL is the base URL for the ChatGPT Codex API.
	ChatGPTBaseURL = "https://chatgpt.com/backend-api/codex"
)

// chatgptLoginTTL is how long a pending login keeps its PKCE verifier for the
// pasted callback; it matches the authorization code's own life.
const chatgptLoginTTL = 5 * time.Minute

// chatgptHTTPTimeout bounds every token endpoint call (exchange and refresh);
// a refresh runs inside the session that triggered it.
const chatgptHTTPTimeout = 30 * time.Second

// ChatGPTOAuth manages the OAuth flow for ChatGPT subscription authentication.
type ChatGPTOAuth struct {
	providers *store.ProviderStore
	// tokenURL is the token endpoint; tests point it at a fake.
	tokenURL string

	mu      sync.Mutex
	pending map[string]*chatgptPending // keyed by state

	// refreshMu serializes refreshes: the refresh token is single-use.
	refreshMu sync.Mutex
}

type chatgptPending struct {
	providerID   string
	codeVerifier string
	// timer drops this entry after chatgptLoginTTL.
	timer *time.Timer
}

// NewChatGPTOAuth returns the OAuth manager over the provider rows it logs in.
func NewChatGPTOAuth(providers *store.ProviderStore) *ChatGPTOAuth {
	if providers == nil {
		panic("providers: NewChatGPTOAuth needs the provider store")
	}
	return &ChatGPTOAuth{
		providers: providers,
		tokenURL:  chatgptTokenURL,
		pending:   make(map[string]*chatgptPending),
	}
}

// chatgptHTTPClient is the token endpoint client, bounded by chatgptHTTPTimeout.
var chatgptHTTPClient = &http.Client{Timeout: chatgptHTTPTimeout}

// ChatGPTLoginResult is returned by StartLogin with the authorize URL. The
// state is not exposed: it rides back inside the callback URL the user pastes
// to CompleteLogin, which is where the server reads it.
type ChatGPTLoginResult struct {
	AuthorizeURL string `json:"authorize_url"`
}

// StartLogin begins the ChatGPT OAuth PKCE flow for the given provider;
// store.ErrNotFound when the provider does not exist.
func (o *ChatGPTOAuth) StartLogin(ctx context.Context, providerID string) (*ChatGPTLoginResult, error) {
	if providerID == "" {
		return nil, fmt.Errorf("provider_id is required")
	}
	pv, err := o.providers.Get(ctx, providerID)
	if err != nil {
		return nil, err
	}
	// Only a chatgpt_login provider can use (and revoke) the token.
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

// CompleteLogin finishes a login begun by StartLogin: it redeems the code in
// the pasted callback URL against the stored PKCE verifier and saves the
// tokens on the provider (decisions §5.41).
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
	// The state binds a callback to its verifier; another provider's flow must
	// not complete this one.
	if p.providerID != providerID {
		return fmt.Errorf("%w: this callback belongs to a different sign-in", ErrChatGPTCallbackInvalid)
	}

	tokens, err := exchangeCode(ctx, chatgptHTTPClient, o.tokenURL, code, p.codeVerifier, chatgptRedirectURI)
	if err != nil {
		// A refusal (4xx) is the code's fault: spent, expired, or another
		// flow's. A 5xx or transport failure stays a server error.
		var te *tokenError
		if errors.As(err, &te) && te.status < 500 {
			return fmt.Errorf("%w: %w", ErrChatGPTCallbackInvalid, err)
		}
		return err
	}
	if err := o.saveTokens(ctx, providerID, tokens); err != nil {
		return err
	}
	// Only a stored token clears the pending entry: a failed exchange leaves
	// the flow for another paste until the TTL drops it.
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

// cleanupPending removes the pending flow for state and stops its expiry
// timer; idempotent (CompleteLogin and the timer both call it).
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

// refreshCredentials rotates the stored token under refreshMu, re-reading the
// row under the lock: a rotation that already landed is reused, not repeated.
func (o *ChatGPTOAuth) refreshCredentials(ctx context.Context, providerID string, tok chatgptTokens) (chatgptTokens, error) {
	o.refreshMu.Lock()
	defer o.refreshMu.Unlock()

	// A racing refresh may have rotated the token while we waited.
	if pv, err := o.providers.Get(ctx, providerID); err == nil {
		var current chatgptTokens
		if json.Unmarshal([]byte(pv.ChatGPTToken), &current) == nil && current.AccessToken != "" {
			if !tokenExpiring(current) {
				return current, nil
			}
			tok = current
		}
	}

	refreshed, err := refreshToken(ctx, chatgptHTTPClient, o.tokenURL, tok.RefreshToken)
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

// chatGPTLoginAvailable reports whether the provider row authenticates by
// chatgpt_login; it gates both StartLogin and saveTokens (the row can change
// during the authorize window).
func chatGPTLoginAvailable(pv *store.Provider) error {
	def, err := DefFor(pv.Type)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrChatGPTLoginUnavailable, err)
	}
	if !slices.Contains(def.AuthModes, AuthModeChatGPTLogin) {
		return fmt.Errorf("%w: not offered by the %s provider — switch the provider type or use an API key", ErrChatGPTLoginUnavailable, def.Type)
	}
	// The ROW's own mode, not only the type's menu.
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

func exchangeCode(ctx context.Context, client *http.Client, tokenURL, code, verifier, redirectURI string) (*chatgptTokens, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {chatgptClientID},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("token exchange request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token exchange request: %w", err)
	}
	defer resp.Body.Close()
	return decodeTokenResponse(resp)
}

func refreshToken(ctx context.Context, client *http.Client, tokenURL, refresh string) (*chatgptTokens, error) {
	body, _ := json.Marshal(map[string]string{
		"client_id":     chatgptClientID,
		"grant_type":    "refresh_token",
		"refresh_token": refresh,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	tok, err := decodeTokenResponse(resp)
	if err != nil {
		return nil, err
	}
	// The endpoint may omit the refresh token when it does not rotate it.
	tok.RefreshToken = cmp.Or(tok.RefreshToken, refresh)
	return tok, nil
}

// tokenError is the token endpoint's refusal: its status and the error code
// and message it carried.
type tokenError struct {
	status        int
	code, message string
}

func (e *tokenError) Error() string {
	head := strconv.Itoa(e.status)
	if e.code != "" {
		head += " " + e.code
	}
	return "token endpoint refused (" + head + "): " + e.message
}

// tokenErrorField decodes the "error" member in either shape the token
// endpoint uses: RFC 6749's string ({"error":"invalid_grant"}) or OpenAI's
// object ({"error":{"code":"token_expired","message":"..."}}).
type tokenErrorField struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (f *tokenErrorField) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*f = tokenErrorField{Code: s}
		return nil
	}
	type plain tokenErrorField // no UnmarshalJSON, so this cannot recurse
	var obj plain
	if err := json.Unmarshal(b, &obj); err != nil {
		return err
	}
	*f = tokenErrorField(obj)
	return nil
}

// tokenResponseCap bounds how much of a token response is read; a real one is
// a few KB.
const tokenResponseCap = 1 << 20

// decodeTokenResponse turns a token endpoint answer into tokens, or into a
// *tokenError when the status or the body says it refused.
func decodeTokenResponse(resp *http.Response) (*chatgptTokens, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, tokenResponseCap))
	if err != nil {
		return nil, fmt.Errorf("reading token response: %w", err)
	}
	var result struct {
		AccessToken  string          `json:"access_token"`
		RefreshToken string          `json:"refresh_token"`
		IDToken      string          `json:"id_token"`
		ExpiresIn    int64           `json:"expires_in"`
		Error        tokenErrorField `json:"error"`
		ErrorDesc    string          `json:"error_description"`
	}
	decodeErr := json.Unmarshal(body, &result)
	if resp.StatusCode < 200 || resp.StatusCode > 299 || result.Error.Code != "" {
		te := &tokenError{status: resp.StatusCode, code: result.Error.Code, message: cmp.Or(result.Error.Message, result.ErrorDesc)}
		if decodeErr != nil || te.message == "" {
			te.message = bodySnippet(body)
		}
		return nil, te
	}
	if decodeErr != nil {
		return nil, fmt.Errorf("decoding token response: %w", decodeErr)
	}
	tok := &chatgptTokens{AccessToken: result.AccessToken, RefreshToken: result.RefreshToken, IDToken: result.IDToken}
	if result.ExpiresIn > 0 {
		tok.ExpiresAt = time.Now().Unix() + result.ExpiresIn
	}
	return tok, nil
}

// bodySnippet is the start of a response body that carried no parseable
// error, for the error message.
func bodySnippet(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
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
