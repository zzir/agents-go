package handler

import (
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// PreviewOrigin decides the browser origin a preview URL opens on. A preview
// serves an untrusted page — someone's dev server — so it must NOT share the
// app's origin, where the workbench token lives in localStorage: the isolated
// origin's scripts cannot read another origin's storage (decisions §5.37).
type PreviewOrigin struct {
	// BaseURL, when set (--preview-base-url), is the fixed origin every grant
	// URL uses — for a reverse proxy that routes a second hostname to the
	// preview listener.
	BaseURL string
	// Port is the preview listener's own port; the origin is derived from the
	// request host and this port when BaseURL is empty.
	Port int
}

// urlFor renders the absolute grant URL. Unconfigured (Port 0, no BaseURL) it
// falls back to a same-origin relative path — the shape unit tests assert; the
// running server always sets a Port, so a real grant is cross-origin.
func (o PreviewOrigin) urlFor(r *http.Request, token string) string {
	tail := server.PreviewPrefix + token + "/"
	switch {
	case o.BaseURL != "":
		return o.BaseURL + tail
	case o.Port == 0:
		return tail
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	// JoinHostPort brackets an IPv6 literal; a bare Sprintf would emit
	// http://::1:9528/… which no browser can open.
	return scheme + "://" + net.JoinHostPort(host, strconv.Itoa(o.Port)) + tail
}

// The port preview: a service running inside a project's sandbox, reachable
// through THIS server rather than published to the world. Publishing the port
// instead would put a member's dev server on every interface of the host,
// collide on a port two people both want, and be unreachable on a remote
// daemon — the gateway has none of those problems and carries the workbench's
// own auth for free (decisions §5.35).

// unreachableHint explains a failed preview by what the port IS. A published
// port is bound on this machine, so routing is not the question — a server
// listening on 127.0.0.1 inside the container is invisible through it. An
// unpublished one is reached across the container network, which the local
// daemon of Docker Desktop does not expose at all.
func unreachableHint(port int, published bool, target string) string {
	head := "could not reach port " + strconv.Itoa(port) + " in the sandbox (" + target + "): "
	if published {
		return head + "the port is published, so either nothing is listening on it, " +
			"or the server inside is bound to 127.0.0.1 — it must listen on 0.0.0.0 to be reachable from outside the container."
	}
	return head + "this port is not published. Add it to the project's ports, or — where the server can reach the " +
		"container network directly — check that something is listening on it."
}

// previewGrantResp is what a mint returns: the URL to open, and when it stops
// working.
type previewGrantResp struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// PreviewGrant mints the short-lived URL a browser tab opens. The URL is on
// the preview origin — a separate origin from the app — so the page it serves
// cannot read the workbench token (decisions §5.37).
//
//	@Summary		Grant a preview URL for a port inside the project's sandbox
//	@Description	Returns a short-lived, unguessable URL under /preview/ on the preview origin. Owner only, and off unless `preview_enabled` is set. A docker template must name a network for its ports to be reachable at all.
//	@Tags			projects
//	@Produce		json
//	@Param			id		path		string	true	"Project id"
//	@Param			port	path		int		true	"Port inside the sandbox"
//	@Success		200		{object}	previewGrantResp
//	@Failure		400		{object}	ErrorResponse
//	@Failure		403		{object}	ErrorResponse	"previews are disabled"
//	@Failure		404		{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/projects/{id}/preview/{port} [post]
func (h *ProjectHandler) PreviewGrant(c *gin.Context) {
	if h.settings == nil || !h.settings.Bool(c.Request.Context(), settings.KeyPreviewEnabled) {
		forbidden(c, "port previews are disabled; an admin turns them on in Settings")
		return
	}
	port, err := strconv.Atoi(c.Param("port"))
	if err != nil || port <= 0 || port > 65535 {
		badRequest(c, "port must be 1-65535")
		return
	}
	p, ok := h.own(c)
	if !ok {
		return
	}
	token, expires := h.grants.mint(p.ID, port, p.OwnerID)
	c.JSON(http.StatusOK, previewGrantResp{URL: h.PreviewOrigin.urlFor(c.Request, token), ExpiresAt: expires})
}

// previewCookie carries the grant token to the sub-resources a previewed page
// fetches by ABSOLUTE path (its /asset.js, a redirect to /login, an HMR
// socket): the preview is one origin, so those requests do not carry the token
// in their path. The tokenized entry point plants it; the cookie route reads
// it. HttpOnly so the untrusted page cannot read the token, scoped to the
// whole origin so every absolute path resolves.
const previewCookie = "preview_token"

// Preview proxies one request into the project's sandbox. It carries NO
// bearer token — a browser tab cannot send one — so the grant (in the path on
// the tokenized entry point, in the cookie for a page's absolute-path
// sub-resources) is the whole authorization, and the route lives outside /api
// where the bearer middleware would otherwise refuse it.
func (h *ProjectHandler) Preview(c *gin.Context) {
	token := c.Param("token")
	fromPath := token != ""
	if !fromPath {
		token, _ = c.Cookie(previewCookie)
	}
	grant, ok := h.grants.resolve(token)
	if !ok {
		c.String(http.StatusNotFound, "this preview link has expired; open a new one from the project menu")
		return
	}
	if h.settings == nil || !h.settings.Bool(c.Request.Context(), settings.KeyPreviewEnabled) {
		c.String(http.StatusForbidden, "port previews are disabled")
		return
	}
	p, err := h.store.Get(c.Request.Context(), grant.projectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Deleted for good: retire every grant on it.
			h.grants.revokeProject(grant.projectID)
			c.String(http.StatusNotFound, "this preview link is no longer valid")
			return
		}
		// A transient lookup failure must not revoke a still-valid grant.
		c.String(http.StatusBadGateway, "the project could not be looked up; try again")
		return
	}
	if p.OwnerID != grant.ownerID {
		// Transferred since the grant was minted: it names a project that is no
		// longer the one it was for.
		h.grants.revokeProject(grant.projectID)
		c.String(http.StatusNotFound, "this preview link is no longer valid")
		return
	}
	// The tokenized entry point plants the grant in a cookie so the page's
	// absolute-path sub-resources resolve to the same grant. One preview origin
	// means one active grant per browser: opening a second project's preview
	// replaces the cookie (decisions §5.35).
	if fromPath {
		http.SetCookie(c.Writer, &http.Cookie{
			Name:     previewCookie,
			Value:    token,
			Path:     "/",
			HttpOnly: true,
			Secure:   c.Request.TLS != nil,
			SameSite: http.SameSiteLaxMode,
			Expires:  grant.expires,
		})
	}
	spec, err := resolveSpec(c.Request.Context(), h.sandboxes, p)
	if err != nil {
		c.String(http.StatusBadGateway, "this project's sandbox cannot be resolved")
		return
	}
	port := grant.port
	// Whether the container publishes this port decides what a failure MEANS,
	// so it is read before the attempt rather than guessed from the error.
	declared, _ := store.DecodeProjectPorts(p.Ports)
	published := slices.Contains(declared, port)
	target, dial, release, err := h.manager.Preview(c.Request.Context(), spec, port)
	if err != nil {
		c.String(http.StatusBadGateway, err.Error())
		return
	}
	defer release()
	base, err := url.Parse(target)
	if err != nil {
		c.String(http.StatusBadGateway, "the preview target is not a URL")
		return
	}
	// The dev server sees the path below the /preview/<token>/ prefix on the
	// tokenized route, and the request path as-is on the cookie route.
	upstreamPath := c.Request.URL.Path
	if fromPath {
		upstreamPath = "/" + strings.TrimPrefix(c.Param("path"), "/")
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(base)
			r.Out.URL.Path = upstreamPath
			r.Out.Host = base.Host
			// Forwarded headers are set deliberately rather than inherited: the
			// workbench's own auth header and every cookie (the grant cookie,
			// and any app cookie that bled across the port-blind cookie scope)
			// must not travel into someone's dev server.
			r.Out.Header.Del("Authorization")
			r.Out.Header.Del("Cookie")
			r.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, perr error) {
			logging.Ctx(r.Context()).Debug("preview upstream failed", "error", perr, "target", target)
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(unreachableHint(port, published, target)))
		},
	}
	if dial != nil {
		// The backend opens the connection: a container's address means nothing
		// on this machine when the daemon is somewhere else. Close this
		// request's upstream conns when it ends — a fresh Transport per request
		// otherwise parks an idle keep-alive conn (and its goroutine, an ssh
		// channel for a remote daemon) that nothing reuses or reaps.
		t := &http.Transport{DialContext: dial}
		defer t.CloseIdleConnections()
		proxy.Transport = t
	}
	proxy.ServeHTTP(c.Writer, c.Request)
}
