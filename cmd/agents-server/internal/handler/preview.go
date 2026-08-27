package handler

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
)

// The port preview: a service running inside a project's sandbox, reachable
// through THIS server rather than published to the world. Publishing the port
// instead would put a member's dev server on every interface of the host,
// collide on a port two people both want, and be unreachable on a remote
// daemon — the gateway has none of those problems and carries the workbench's
// own auth for free (decisions §5.35).

// previewGrantResp is what a mint returns: the URL to open, and when it stops
// working.
type previewGrantResp struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// PreviewGrant mints the one-time URL a browser tab opens.
//
//	@Summary		Grant a preview URL for a port inside the project's sandbox
//	@Description	Returns a short-lived, unguessable URL under /preview/. Owner only, and off unless `preview_enabled` is set. A docker template must name a network for its ports to be reachable at all.
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
	c.JSON(http.StatusOK, previewGrantResp{URL: server.PreviewPrefix + token + "/", ExpiresAt: expires})
}

// Preview proxies one request into the project's sandbox. It carries NO
// bearer token — a browser tab cannot send one — so the grant in its path is
// the whole authorization, and the route lives outside /api where the bearer
// middleware would otherwise refuse it.
func (h *ProjectHandler) Preview(c *gin.Context) {
	grant, ok := h.grants.resolve(c.Param("token"))
	if !ok {
		c.String(http.StatusNotFound, "this preview link has expired; open a new one from the project menu")
		return
	}
	if h.settings == nil || !h.settings.Bool(c.Request.Context(), settings.KeyPreviewEnabled) {
		c.String(http.StatusForbidden, "port previews are disabled")
		return
	}
	p, err := h.store.Get(c.Request.Context(), grant.projectID)
	if err != nil || p.OwnerID != grant.ownerID {
		// The project was deleted or transferred since the grant: the grant
		// names a project that is no longer the one it was minted for.
		h.grants.revokeProject(grant.projectID)
		c.String(http.StatusNotFound, "this preview link is no longer valid")
		return
	}
	spec, err := resolveSpec(c.Request.Context(), h.sandboxes, p)
	if err != nil {
		c.String(http.StatusBadGateway, "this project's sandbox cannot be resolved")
		return
	}
	port := grant.port
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

	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(base)
			// The service sees the path BELOW the preview prefix, so a dev
			// server's own routes match without knowing it is proxied.
			r.Out.URL.Path = "/" + strings.TrimPrefix(c.Param("path"), "/")
			r.Out.Host = base.Host
			// Forwarded headers are set deliberately rather than inherited:
			// the workbench's own auth header must not travel into someone's
			// dev server.
			r.Out.Header.Del("Authorization")
			r.Out.Header.Del("Cookie")
			r.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, perr error) {
			logging.Ctx(r.Context()).Debug("preview upstream failed", "error", perr, "target", target)
			w.WriteHeader(http.StatusBadGateway)
			// Two causes look identical from here, and a person needs both:
			// nothing is listening, or this machine cannot route to the
			// container at all (Docker Desktop keeps its container network
			// inside a VM — see decisions §5.35).
			_, _ = w.Write([]byte("could not reach port " + strconv.Itoa(port) + " in the sandbox (" + target + "): " +
				"either nothing is listening on it, or this server cannot route to the sandbox's network — " +
				"which is the case for Docker Desktop's local daemon on macOS and Windows."))
		},
	}
	if dial != nil {
		// The backend opens the connection: a container's address means
		// nothing on this machine when the daemon is somewhere else.
		proxy.Transport = &http.Transport{DialContext: dial}
	}
	proxy.ServeHTTP(c.Writer, c.Request)
}
