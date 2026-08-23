package handler

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"html"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/mcpservers"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// McpServerHandler serves CRUD and connection endpoints for MCP servers.
type McpServerHandler struct {
	store   *store.McpServerStore
	manager *mcpservers.Manager
	oauth   *mcpservers.OAuthCoordinator
	// baseURL is the configured public origin (--base-url); empty means derive
	// from the direct connection.
	baseURL string
}

// mcpOAuthCallbackPath is the MCP OAuth redirect route, relative to the API
// group. It must stay a path server.TokenAuth exempts — a browser redirect
// carries no bearer, so anywhere else under the API answers 401.
const mcpOAuthCallbackPath = "/mcp-servers/oauth/callback"

// NewMcpServerHandler returns a handler backed by the given store and connection manager.
func NewMcpServerHandler(s *store.McpServerStore, m *mcpservers.Manager, oc *mcpservers.OAuthCoordinator, baseURL string) *McpServerHandler {
	return &McpServerHandler{store: s, manager: m, oauth: oc, baseURL: baseURL}
}

// MCP server lifecycle states reported to the UI. Exactly one is derived per
// server (see status) — the frontend renders it verbatim and keeps no state
// model of its own.
const (
	mcpStatusDisabled     = "disabled"     // enabled=false; only the toggle applies
	mcpStatusConnected    = "connected"    // live connection established
	mcpStatusAuthorizing  = "authorizing"  // OAuth popup pending user action
	mcpStatusConnecting   = "connecting"   // handshake in flight
	mcpStatusNeedsAuth    = "needs_auth"   // OAuth without a token: user must authorize
	mcpStatusDisconnected = "disconnected" // enabled but no live connection
)

type mcpServerListItem struct {
	store.McpServerConfig
	// Status is the single derived lifecycle state: disabled, connecting,
	// authorizing, needs_auth, disconnected, or connected.
	Status string `json:"status"`
	// HasOAuthToken reports whether a persisted OAuth token exists. It gates
	// the "clear auth" action independently of the lifecycle status.
	HasOAuthToken bool `json:"has_oauth_token,omitempty"`
}

// listItem assembles the API view of a server: sanitized config plus the
// derived lifecycle status.
func (h *McpServerHandler) listItem(cfg *store.McpServerConfig) mcpServerListItem {
	return mcpServerListItem{
		McpServerConfig: sanitizeMcpConfig(*cfg),
		Status:          h.status(cfg),
		HasOAuthToken:   cfg.OAuthToken != "",
	}
}

// status derives the single lifecycle state the UI renders, from stored config
// and live connection state. Order matters: disabled wins over everything;
// authorizing is checked before connecting because an interactive OAuth flow
// holds the manager's connect slot for its whole popup wait, so IsConnecting
// stays true throughout and the more specific state must win.
func (h *McpServerHandler) status(cfg *store.McpServerConfig) string {
	switch {
	case !cfg.Enabled:
		return mcpStatusDisabled
	case h.manager.IsConnected(cfg.ID):
		return mcpStatusConnected
	case h.oauth.IsAuthorizing(cfg.ID):
		return mcpStatusAuthorizing
	case h.manager.IsConnecting(cfg.ID):
		return mcpStatusConnecting
	case mcpservers.IsOAuthConfig(cfg) && cfg.OAuthToken == "":
		return mcpStatusNeedsAuth
	default:
		return mcpStatusDisconnected
	}
}

// List responds with all MCP server configurations and their connection state.
//
//	@Summary	List MCP servers
//	@Tags		mcp-servers
//	@Produce	json
//	@Success	200	{array}		mcpServerListItem
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/mcp-servers [get]
func (h *McpServerHandler) List(c *gin.Context) {
	configs, err := h.store.List(c.Request.Context())
	if err != nil {
		internalError(c, err)
		return
	}
	items := make([]mcpServerListItem, len(configs))
	for i := range configs {
		items[i] = h.listItem(&configs[i])
	}
	c.JSON(http.StatusOK, items)
}

// mcpServerReq is the request body for both Create and Update.
type mcpServerReq struct {
	Name          string `json:"name"`
	TransportType string `json:"transport_type"`
	Enabled       bool   `json:"enabled"`
	// Config is the transport-specific settings object, interpreted per
	// TransportType (see store.StdioMcpConfig / store.HTTPMcpConfig).
	Config json.RawMessage `json:"config"`
}

func (r *mcpServerReq) toModel() *store.McpServerConfig {
	return &store.McpServerConfig{
		Name:          r.Name,
		TransportType: r.TransportType,
		Enabled:       r.Enabled,
		Config:        r.Config,
	}
}

func (r *mcpServerReq) validate() string {
	if r.Name == "" {
		return "name is required"
	}
	// Validate transport + its config here so a broken server can't sit in the
	// DB looking configured until the first connect attempt fails.
	switch r.TransportType {
	case "stdio":
		var sc store.StdioMcpConfig
		if len(r.Config) > 0 {
			if err := json.Unmarshal(r.Config, &sc); err != nil {
				return "config is not valid JSON: " + err.Error()
			}
		}
		if sc.Command == "" {
			return "stdio transport requires config.command"
		}
	case "streamable_http":
		var hc store.HTTPMcpConfig
		if len(r.Config) > 0 {
			if err := json.Unmarshal(r.Config, &hc); err != nil {
				return "config is not valid JSON: " + err.Error()
			}
		}
		if hc.Endpoint == "" {
			return "streamable_http transport requires config.endpoint"
		}
	case "":
		return "transport_type is required"
	default:
		return "transport_type must be stdio or streamable_http, got " + r.TransportType
	}
	return ""
}

// Create persists a new MCP server configuration from the request body.
//
//	@Summary		Create MCP server
//	@Description	config is transport-specific: {command, args} for stdio; {endpoint, headers, auth_mode, oauth_*} for streamable_http. Header values and oauth_client_secret are write-only (******** mask semantics).
//	@Tags			mcp-servers
//	@Accept			json
//	@Produce		json
//	@Param			server	body		mcpServerReq	true	"MCP server configuration"
//	@Success		201		{object}	mcpServerListItem
//	@Failure		400		{object}	ErrorResponse
//	@Failure		500		{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/mcp-servers [post]
func (h *McpServerHandler) Create(c *gin.Context) {
	var req mcpServerReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err.Error())
		return
	}
	if msg := req.validate(); msg != "" {
		badRequest(c, msg)
		return
	}
	cfg := req.toModel()
	// No stored config yet: mask sentinels resolve to empty.
	cfg.Config = restoreMcpConfig(cfg.TransportType, cfg.Config, nil)
	if err := h.store.Create(c.Request.Context(), cfg); err != nil {
		saveError(c, err) // duplicate name -> 409
		return
	}
	// A newly created enabled server connects in the background, same as an
	// update — "I added a server" should not need a separate connect click.
	h.manager.Reconcile(cfg, h.oauth)
	created(c, cfg.ID, h.listItem(cfg))
}

// Get responds with the MCP server configuration identified by the id path parameter.
//
//	@Summary	Get MCP server
//	@Tags		mcp-servers
//	@Produce	json
//	@Param		id	path		string	true	"MCP server ID"
//	@Success	200	{object}	mcpServerListItem
//	@Failure	404	{object}	ErrorResponse
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/mcp-servers/{id} [get]
func (h *McpServerHandler) Get(c *gin.Context) {
	cfg, err := h.store.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		storeError(c, err)
		return
	}
	c.JSON(http.StatusOK, h.listItem(cfg))
}

// Update overwrites the MCP server configuration identified by the id path
// parameter and responds with the updated item. When enabled flips to false,
// the server is disconnected; when flipped to true, a connection attempt is
// made automatically.
//
//	@Summary		Update MCP server
//	@Description	Full replace; flipping enabled disconnects/reconnects the server. Masked secrets keep their stored values.
//	@Tags			mcp-servers
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string			true	"MCP server ID"
//	@Param			server	body		mcpServerReq	true	"MCP server configuration"
//	@Success		200		{object}	mcpServerListItem
//	@Failure		400		{object}	ErrorResponse
//	@Failure		404		{object}	ErrorResponse
//	@Failure		500		{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/mcp-servers/{id} [put]
func (h *McpServerHandler) Update(c *gin.Context) {
	var req mcpServerReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err.Error())
		return
	}
	if msg := req.validate(); msg != "" {
		badRequest(c, msg)
		return
	}
	ctx := c.Request.Context()
	id := c.Param("id")
	cfg := req.toModel()
	// Masked header values / oauth_client_secret round-trip to their stored
	// values inside the store's transaction.
	err := h.store.Update(ctx, id, cfg, func(prev *store.McpServerConfig) error {
		cfg.Config = restoreMcpConfig(cfg.TransportType, cfg.Config, prev.Config)
		return nil
	})
	if err != nil {
		saveError(c, err) // duplicate name -> 409, not-found -> 404
		return
	}
	updated, err := h.store.Get(ctx, id)
	if err != nil {
		storeError(c, err)
		return
	}
	// Make the live connection match the newly persisted config (drop stale,
	// reconnect an enabled server in the background). The manager owns the
	// ordering; the response status typically reads "connecting".
	h.manager.Reconcile(updated, h.oauth)
	c.JSON(http.StatusOK, h.listItem(updated))
}

// Delete disconnects and removes the MCP server identified by the id path parameter.
//
//	@Summary	Delete MCP server
//	@Tags		mcp-servers
//	@Param		id	path	string	true	"MCP server ID"
//	@Success	204	"deleted"
//	@Failure	404	{object}	ErrorResponse
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/mcp-servers/{id} [delete]
func (h *McpServerHandler) Delete(c *gin.Context) {
	id := c.Param("id")
	// Delete the row first, then disconnect: a failed delete must not leave a
	// persisted server whose live connection has already been torn down.
	if err := h.store.Delete(c.Request.Context(), id); err != nil {
		storeError(c, err)
		return
	}
	_ = h.manager.Disconnect(id)
	c.Status(http.StatusNoContent)
}

// mcpConnectResp is the Connect response: status "connected", or
// "authorization_required" with the URL to open in a popup.
type mcpConnectResp struct {
	Status       string `json:"status"`
	AuthorizeURL string `json:"authorize_url,omitempty"`
}

// Connect opens a connection to the MCP server identified by the id path parameter.
// For OAuth-enabled servers, it may return an authorize_url instead of connecting
// directly; the frontend should open that URL in a popup and wait for the callback.
//
//	@Summary		Connect MCP server
//	@Description	Establishes the connection. OAuth-enabled servers may answer with status "authorization_required" and an authorize_url to open in a browser popup. Disabled servers cannot be connected (409).
//	@Tags			mcp-servers
//	@Produce		json
//	@Param			id	path		string	true	"MCP server ID"
//	@Success		200	{object}	mcpConnectResp
//	@Failure		404	{object}	ErrorResponse
//	@Failure		409	{object}	ErrorResponse	"server is disabled"
//	@Failure		502	{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/mcp-servers/{id}/connect [post]
func (h *McpServerHandler) Connect(c *gin.Context) {
	cfg, err := h.store.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		storeError(c, err)
		return
	}
	// A disabled server must never gain a live connection: agents pick tools by
	// connection state, so connecting one would put its tools back in play and
	// silently void the disable switch.
	if !cfg.Enabled {
		conflict(c, "server is disabled; enable it before connecting")
		return
	}

	if cfg.TransportType == "streamable_http" {
		var hc store.HTTPMcpConfig
		if err := json.Unmarshal(cfg.Config, &hc); err == nil && hc.AuthMode == "oauth" {
			redirectURI := h.externalOrigin(c.Request) + server.APIPrefix + mcpOAuthCallbackPath
			result, err := h.oauth.ConnectWithOAuth(c.Request.Context(), h.manager, cfg, &hc, redirectURI)
			if err != nil {
				upstreamError(c, err)
				return
			}
			if result.Connected {
				c.JSON(http.StatusOK, mcpConnectResp{Status: "connected"})
			} else {
				c.JSON(http.StatusOK, mcpConnectResp{
					Status:       "authorization_required",
					AuthorizeURL: result.AuthorizeURL,
				})
			}
			return
		}
	}

	if err := h.manager.Connect(c.Request.Context(), cfg); err != nil {
		upstreamError(c, err)
		return
	}
	c.JSON(http.StatusOK, mcpConnectResp{Status: "connected"})
}

// externalOrigin is the browser-facing origin (scheme://host) used to build
// the OAuth redirect_uri: --base-url when configured, otherwise the direct
// connection. Forwarded/X-Forwarded-* headers are deliberately NOT consulted —
// a direct client can forge them — so behind a TLS-terminating reverse proxy
// --base-url is required for OAuth flows to build a matching redirect_uri.
func (h *McpServerHandler) externalOrigin(r *http.Request) string {
	if h.baseURL != "" {
		return h.baseURL
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// ClearOAuth disconnects the MCP server identified by the id path parameter
// and removes its persisted OAuth token, forcing a fresh authorization on the
// next connect. This is the "sign out" action for OAuth-enabled servers.
//
//	@Summary	Clear MCP OAuth token
//	@Tags		mcp-servers
//	@Param		id	path	string	true	"MCP server ID"
//	@Success	204	"token cleared"
//	@Failure	404	{object}	ErrorResponse
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/mcp-servers/{id}/oauth-token [delete]
func (h *McpServerHandler) ClearOAuth(c *gin.Context) {
	id := c.Param("id")
	if !requireResource(c, h.store.Get, id) {
		return
	}
	if err := h.manager.Disconnect(id); err != nil {
		internalError(c, err)
		return
	}
	if err := h.store.ClearOAuthToken(c.Request.Context(), id); err != nil {
		internalError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// OAuthCallback handles the OAuth redirect from the authorization server.
// It delivers the authorization code to the pending connection and renders a
// small HTML page that notifies the opener window via postMessage.
//
//	@Summary		MCP OAuth callback
//	@Description	Browser-facing redirect target of the OAuth flow (no auth token required). Renders an HTML result page, not JSON.
//	@Tags			mcp-servers
//	@Produce		html
//	@Param			state	query		string	true	"OAuth state"
//	@Param			code	query		string	true	"Authorization code"
//	@Param			iss		query		string	false	"RFC 9207 issuer identifier"
//	@Param			error	query		string	false	"Provider error, e.g. access_denied"
//	@Success		200		{string}	string	"HTML result page"
//	@Failure		400		{string}	string	"missing state or code parameter"
//	@Router			/mcp-servers/oauth/callback [get]
func (h *McpServerHandler) OAuthCallback(c *gin.Context) {
	// The request logger records only the path (the query is redacted), so the
	// outcome of the callback — the actionable half — is logged here explicitly.
	log := logging.Ctx(c.Request.Context())
	// Provider denial redirects (?error=access_denied&state=...) carry no
	// code, so the error parameter must be checked before requiring one.
	if errMsg := c.Query("error"); errMsg != "" {
		log.Warn("mcp oauth callback: authorization server returned an error", "error", errMsg)
		writeOAuthCallbackPage(c, "error", errMsg)
		return
	}
	state := c.Query("state")
	code := c.Query("code")
	if state == "" || code == "" {
		log.Warn("mcp oauth callback: missing state or code parameter")
		c.String(http.StatusBadRequest, "missing state or code parameter")
		return
	}
	if err := h.oauth.HandleCallback(state, code, c.Query("iss")); err != nil {
		log.Warn("mcp oauth callback: could not deliver authorization code", "error", err)
		writeOAuthCallbackPage(c, "error", err.Error())
		return
	}
	log.Info("mcp oauth callback: authorization code delivered to the pending connection")
	writeOAuthCallbackPage(c, "success", "")
}

// oauthCallbackScript is the static popup script: it reads the flow status
// from the body's data attribute, notifies the opener, and closes the window.
// It must stay constant so its CSP hash below stays valid.
const oauthCallbackScript = `var s=document.body.getAttribute("data-status")||"error";
if(window.opener){window.opener.postMessage({type:"mcp-oauth-done",status:s},location.origin);}
setTimeout(function(){window.close();},1500);`

var oauthCallbackScriptHash = func() string {
	sum := sha256.Sum256([]byte(oauthCallbackScript))
	return base64.StdEncoding.EncodeToString(sum[:])
}()

// writeOAuthCallbackPage renders the OAuth popup result page. The global CSP
// blocks inline scripts, so the response carries its own policy allowing
// exactly the static script above by hash. status is an internal constant
// ("success"/"error"); errMsg may echo attacker-influenced query input and is
// HTML-escaped.
func writeOAuthCallbackPage(c *gin.Context, status, errMsg string) {
	msg := "Authorization successful. You can close this window."
	if status != "success" {
		msg = "Authorization failed: " + errMsg
	}
	page := `<!DOCTYPE html><html><body data-status="` + status + `"><p id="msg">` +
		html.EscapeString(msg) +
		`</p><script>` + oauthCallbackScript + `</script></body></html>`
	c.Header("Content-Security-Policy",
		"default-src 'self'; script-src 'sha256-"+oauthCallbackScriptHash+"'")
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.String(http.StatusOK, page)
}

type mcpToolInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Tools responds with the tools exposed by the connected MCP server. A
// disconnected server is a 409: the request is fine, the server state isn't.
//
//	@Summary	List MCP server tools
//	@Tags		mcp-servers
//	@Produce	json
//	@Param		id	path		string	true	"MCP server ID"
//	@Success	200	{array}		mcpToolInfo
//	@Failure	409	{object}	ErrorResponse	"server not connected"
//	@Failure	502	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/mcp-servers/{id}/tools [get]
func (h *McpServerHandler) Tools(c *gin.Context) {
	id := c.Param("id")
	// Distinguish "no such server" (404) from "exists but not connected" (409):
	// querying the manager alone can't tell them apart.
	if !requireResource(c, h.store.Get, id) {
		return
	}
	srv := h.manager.Get(id)
	if srv == nil {
		conflict(c, "server not connected")
		return
	}
	tools, err := srv.ListTools(c.Request.Context(), nil, nil)
	if err != nil {
		upstreamError(c, err)
		return
	}
	items := make([]mcpToolInfo, len(tools))
	for i, t := range tools {
		items[i] = mcpToolInfo{Name: t.Name, Description: t.Description}
	}
	c.JSON(http.StatusOK, items)
}
