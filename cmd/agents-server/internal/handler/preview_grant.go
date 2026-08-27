package handler

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

// A preview grant is how a BROWSER TAB reaches a port inside a sandbox. Every
// other route here authenticates with a bearer token, which a tab cannot
// carry: opening a URL sends no header. So the owner asks for a grant through
// the authenticated API, and the grant — an unguessable, short-lived,
// single-project token — is the authorization the preview path checks
// (decisions §5.35).
//
// Grants live in memory. A restart invalidating them is the right behavior
// (the preview is a live view of a live sandbox) and it keeps the feature
// free of a signing key that would have to exist even in the single-user
// mode where nothing else is sealed.

// previewGrantTTL is how long a minted grant stays usable. Long enough to
// read a page and click around, short enough that a leaked URL stops working
// before it is worth passing on.
const previewGrantTTL = 30 * time.Minute

type previewGrant struct {
	projectID string
	port      int
	ownerID   string
	expires   time.Time
}

// previewGrants is the live set, swept on every mint so an idle server does
// not hold yesterday's tokens.
type previewGrants struct {
	mu     sync.Mutex
	tokens map[string]previewGrant
}

func newPreviewGrants() *previewGrants {
	return &previewGrants{tokens: map[string]previewGrant{}}
}

// mint returns a fresh token for (project, port).
func (g *previewGrants) mint(projectID string, port int, ownerID string) (string, time.Time) {
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	token := base64.RawURLEncoding.EncodeToString(raw)
	expires := time.Now().Add(previewGrantTTL)
	g.mu.Lock()
	defer g.mu.Unlock()
	for t, grant := range g.tokens {
		if time.Now().After(grant.expires) {
			delete(g.tokens, t)
		}
	}
	g.tokens[token] = previewGrant{projectID: projectID, port: port, ownerID: ownerID, expires: expires}
	return token, expires
}

// resolve returns the grant a token names, or false when it is unknown or
// expired. An expired token is deleted on the way out.
func (g *previewGrants) resolve(token string) (previewGrant, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	grant, ok := g.tokens[token]
	if !ok {
		return previewGrant{}, false
	}
	if time.Now().After(grant.expires) {
		delete(g.tokens, token)
		return previewGrant{}, false
	}
	return grant, true
}

// revokeProject drops every grant on a project — its delete, or a
// configuration change that replaced the container the grants pointed into.
func (g *previewGrants) revokeProject(projectID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for t, grant := range g.tokens {
		if grant.projectID == projectID {
			delete(g.tokens, t)
		}
	}
}
