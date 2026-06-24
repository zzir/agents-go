package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"golang.org/x/oauth2"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// tokenPayload is the JSON representation of an oauth2.Token for persistence.
type tokenPayload struct {
	AccessToken  string    `json:"access_token"`
	TokenType    string    `json:"token_type"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Expiry       time.Time `json:"expiry,omitempty"`
}

// persistentOAuthHandler wraps an auth.OAuthHandler and persists the token
// after a successful Authorize. On construction it may be pre-populated with
// a saved token so the user doesn't need to re-authorize after a server restart.
type persistentOAuthHandler struct {
	inner    auth.OAuthHandler
	configID string
	store    *store.McpServerStore

	mu          sync.Mutex
	tokenSource oauth2.TokenSource
}

func newPersistentOAuthHandler(inner auth.OAuthHandler, configID string, s *store.McpServerStore, savedToken string) *persistentOAuthHandler {
	h := &persistentOAuthHandler{
		inner:    inner,
		configID: configID,
		store:    s,
	}
	if savedToken != "" {
		if tok := decodeToken(savedToken); tok != nil && tok.Valid() {
			h.tokenSource = oauth2.StaticTokenSource(tok)
		}
	}
	return h
}

func (h *persistentOAuthHandler) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	h.mu.Lock()
	ts := h.tokenSource
	h.mu.Unlock()
	if ts != nil {
		return ts, nil
	}
	return h.inner.TokenSource(ctx)
}

func (h *persistentOAuthHandler) Authorize(ctx context.Context, req *http.Request, resp *http.Response) error {
	if err := h.inner.Authorize(ctx, req, resp); err != nil {
		return err
	}
	ts, err := h.inner.TokenSource(ctx)
	if err != nil || ts == nil {
		return nil
	}
	tok, err := ts.Token()
	if err != nil || tok == nil {
		return nil
	}
	h.mu.Lock()
	h.tokenSource = ts
	h.mu.Unlock()

	if encoded := encodeToken(tok); encoded != "" {
		_ = h.store.SaveOAuthToken(context.Background(), h.configID, encoded)
	}
	return nil
}

func encodeToken(tok *oauth2.Token) string {
	p := tokenPayload{
		AccessToken:  tok.AccessToken,
		TokenType:    tok.TokenType,
		RefreshToken: tok.RefreshToken,
		Expiry:       tok.Expiry,
	}
	b, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	return string(b)
}

func decodeToken(s string) *oauth2.Token {
	var p tokenPayload
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		return nil
	}
	return &oauth2.Token{
		AccessToken:  p.AccessToken,
		TokenType:    p.TokenType,
		RefreshToken: p.RefreshToken,
		Expiry:       p.Expiry,
	}
}

var _ auth.OAuthHandler = (*persistentOAuthHandler)(nil)
