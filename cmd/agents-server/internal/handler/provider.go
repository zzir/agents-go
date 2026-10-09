package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	anthropicsdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/gin-gonic/gin"
	openaisdk "github.com/openai/openai-go/v3"

	"github.com/zzir/agents-go/cmd/agents-server/internal/providers"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// ProviderHandler serves CRUD endpoints for provider endpoints and their
// credentials — the ONLY surface a model-API key crosses.
type ProviderHandler struct {
	store  *store.ProviderStore
	models modelListCache
}

// NewProviderHandler returns a handler backed by the given store.
func NewProviderHandler(s *store.ProviderStore) *ProviderHandler {
	return &ProviderHandler{store: s}
}

// modelListCache keeps a provider's live model list for modelListTTL, keyed by
// the row and its last change, so an edited key asks again.
type modelListCache struct {
	mu      sync.Mutex
	entries map[string]modelListEntry
}

type modelListEntry struct {
	models []providers.ModelInfo
	at     time.Time
}

const modelListTTL = 10 * time.Minute

func (c *modelListCache) get(key string) ([]providers.ModelInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || time.Since(e.at) > modelListTTL {
		return nil, false
	}
	return e.models, true
}

func (c *modelListCache) put(key string, models []providers.ModelInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]modelListEntry{}
	}
	c.entries[key] = modelListEntry{models: models, at: time.Now()}
}

// errModelListNeedsKey is a listing asked of a provider with no API key to
// ask with — a ChatGPT login's token does not list models.
var errModelListNeedsKey = errors.New("model listing needs an API key")

// listModels answers the provider's live model list, from the cache when
// fresh; the error is the backend's, with the key never in it.
func (h *ProviderHandler) listModels(ctx context.Context, pv *store.Provider) ([]providers.ModelInfo, error) {
	if pv.AuthMode == providers.AuthModeChatGPTLogin || pv.APIKey == "" {
		return nil, errModelListNeedsKey
	}
	def, err := providers.DefFor(pv.Type)
	if err != nil || def.ListModels == nil {
		return nil, fmt.Errorf("provider type %q lists no models", pv.Type)
	}
	key := pv.ID + "@" + pv.UpdatedAt.UTC().Format(time.RFC3339Nano)
	if models, ok := h.models.get(key); ok {
		return models, nil
	}
	models, err := def.ListModels(ctx, pv.APIKey, pv.BaseURL)
	if err != nil {
		return nil, providerFailure(err)
	}
	if models == nil {
		models = []providers.ModelInfo{}
	}
	h.models.put(key, models)
	return models, nil
}

// providerFailure is what a failed listing tells the caller: the status the
// provider answered or that it was unreachable — never the URL, body or key.
func providerFailure(err error) error {
	if oe, ok := errors.AsType[*openaisdk.Error](err); ok && oe.Response != nil {
		return fmt.Errorf("the provider answered %d %s", oe.Response.StatusCode, http.StatusText(oe.Response.StatusCode))
	}
	if ae, ok := errors.AsType[*anthropicsdk.Error](err); ok && ae.Response != nil {
		return fmt.Errorf("the provider answered %d %s", ae.Response.StatusCode, http.StatusText(ae.Response.StatusCode))
	}
	if strings.Contains(strings.ToLower(err.Error()), "context deadline exceeded") {
		return errors.New("the endpoint did not answer in time")
	}
	return errors.New("the endpoint could not be reached")
}

// Models answers the provider's live model list.
//
//	@Summary		List a provider's models
//	@Description	Asks the provider which models the stored key may use — its live answer, cached for ten minutes per saved row, never kept in the database. An OpenAI-shaped backend names models only; an Anthropic one adds the context window, the output ceiling and the thinking forms. 409 for a provider with no API key to ask with (a ChatGPT login), 502 when the provider refuses or cannot be reached.
//	@Tags			providers
//	@Produce		json
//	@Param			id	path		string	true	"Provider ID"
//	@Success		200	{array}		providers.ModelInfo
//	@Failure		404	{object}	ErrorResponse
//	@Failure		409	{object}	ErrorResponse	"no API key to list with"
//	@Failure		502	{object}	ErrorResponse	"the provider refused or is unreachable"
//	@Security		BearerAuth
//	@Router			/providers/{id}/models [get]
func (h *ProviderHandler) Models(c *gin.Context) {
	pv, ok := gatedRow(c, h.store.CrudStore, providerScope, visibleRow)
	if !ok {
		return
	}
	models, err := h.listModels(c.Request.Context(), pv)
	if err != nil {
		if errors.Is(err, errModelListNeedsKey) {
			conflict(c, err.Error())
			return
		}
		upstreamError(c, err)
		return
	}
	c.JSON(http.StatusOK, models)
}

// providerTestReq is Test's optional body.
type providerTestReq struct {
	// Model, when given, is checked against the listing.
	Model string `json:"model,omitempty"`
}

// providerTestResp is what a provider test answers.
type providerTestResp struct {
	OK bool `json:"ok"`
	// Detail says what was checked; it never carries the key.
	Detail string `json:"detail,omitempty"`
	// ModelCount is how many models the provider listed.
	ModelCount int `json:"model_count,omitempty"`
	// ModelFound, with a model in the request, says whether the listing names it.
	ModelFound *bool `json:"model_found,omitempty"`
}

// Test checks the provider's key and endpoint by listing its models.
//
//	@Summary		Test a provider
//	@Description	Lists the provider's models with the stored key — no tokens are spent — and reports how many it has; with a model in the body, whether the listing names it. A ChatGPT-login provider reports whether it is logged in instead. 502 when the key is refused or the endpoint cannot be reached; the detail never repeats the key.
//	@Tags			providers
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string			true	"Provider ID"
//	@Param			body	body		providerTestReq	false	"A model to look for"
//	@Success		200		{object}	providerTestResp
//	@Failure		404		{object}	ErrorResponse
//	@Failure		502		{object}	ErrorResponse	"the key was refused or the endpoint is unreachable"
//	@Security		BearerAuth
//	@Router			/providers/{id}/test [post]
func (h *ProviderHandler) Test(c *gin.Context) {
	pv, ok := gatedRow(c, h.store.CrudStore, providerScope, visibleRow)
	if !ok {
		return
	}
	var req providerTestReq
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			badRequest(c, err.Error())
			return
		}
	}
	if pv.AuthMode == providers.AuthModeChatGPTLogin {
		if pv.ChatGPTToken == "" {
			c.JSON(http.StatusOK, providerTestResp{OK: false, Detail: "chatgpt_not_logged_in"})
			return
		}
		c.JSON(http.StatusOK, providerTestResp{OK: true, Detail: "chatgpt_logged_in"})
		return
	}
	models, err := h.listModels(c.Request.Context(), pv)
	if err != nil {
		upstreamError(c, err)
		return
	}
	resp := providerTestResp{OK: true, Detail: fmt.Sprintf("listed %d models", len(models)), ModelCount: len(models)}
	if req.Model != "" {
		found := false
		for _, m := range models {
			if m.ID == req.Model {
				found = true
				break
			}
		}
		resp.ModelFound = &found
		if !found {
			resp.Detail += "; " + req.Model + " is not among them"
		}
	}
	c.JSON(http.StatusOK, resp)
}

// providerReq is the request body for Create and Update; the id, the
// timestamps and the ChatGPT token are the server's.
type providerReq struct {
	Name     string `json:"name"`
	Type     string `json:"type,omitempty"`
	AuthMode string `json:"auth_mode,omitempty"`
	// APIKey is write-only: the ******** mask keeps the stored key.
	APIKey  string `json:"api_key,omitempty"`
	BaseURL string `json:"base_url,omitempty"`
	// Scope on create only: "global" (admin) or the "private" default; an
	// update never moves it (POST /:id/scope does).
	Scope string `json:"scope,omitempty"`
}

func (r *providerReq) toModel() *store.Provider {
	return &store.Provider{Name: r.Name, Type: r.Type, AuthMode: r.AuthMode, APIKey: r.APIKey, BaseURL: r.BaseURL, Scope: r.Scope}
}

// bind decodes and validates an incoming provider body, reporting the failure
// itself. Type and auth mode are the provider registry's answer.
func (h *ProviderHandler) bind(c *gin.Context) (*store.Provider, bool) {
	var req providerReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err.Error())
		return nil, false
	}
	pv := req.toModel()
	if err := store.NormalizeProvider(pv); err != nil {
		badRequest(c, err.Error())
		return nil, false
	}
	if err := providers.Validate(pv); err != nil {
		badRequest(c, err.Error())
		return nil, false
	}
	return pv, true
}

// List responds with every provider, keys masked.
//
//	@Summary	List providers
//	@Tags		providers
//	@Produce	json
//	@Success	200	{array}		store.Provider
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/providers [get]
func (h *ProviderHandler) List(c *gin.Context) {
	list, ok := listVisible(c, h.store.CrudStore)
	if !ok {
		return
	}
	for i := range list {
		sanitizeProvider(&list[i])
	}
	c.JSON(http.StatusOK, list)
}

// Get responds with one provider, key masked.
//
//	@Summary	Get a provider
//	@Tags		providers
//	@Produce	json
//	@Param		id	path		string	true	"Provider ID"
//	@Success	200	{object}	store.Provider
//	@Failure	404	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/providers/{id} [get]
func (h *ProviderHandler) Get(c *gin.Context) {
	pv, ok := gatedRow(c, h.store.CrudStore, providerScope, visibleRow)
	if !ok {
		return
	}
	sanitizeProvider(pv)
	c.JSON(http.StatusOK, pv)
}

// providerScope reads a provider's (scope, owner) pair for the scoped-CRUD gates.
func providerScope(p *store.Provider) (string, string) { return p.Scope, p.OwnerID }

// scopeReq is the body of every POST /:id/scope — the promotion/demotion act
// (decisions §5.29): promote is admin-only, demote is the admin's or the owner's.
type scopeReq struct {
	Scope string `json:"scope" binding:"required"`
}

// bindScope decodes and validates a scope-change body.
func bindScope(c *gin.Context) (string, bool) {
	var req scopeReq
	if err := c.ShouldBindJSON(&req); err != nil || (req.Scope != store.ScopeGlobal && req.Scope != store.ScopePrivate) {
		badRequest(c, `scope must be "global" or "private"`)
		return "", false
	}
	return req.Scope, true
}

// sameScope refuses a /scope request naming the scope the row already holds
// (decisions §5.29). Writes the 409 and returns true when refused.
func sameScope(c *gin.Context, kind, current, requested string) bool {
	if current == requested {
		conflict(c, kind+" is already "+requested)
		return true
	}
	return false
}

// SetScope promotes a provider to global or demotes it back to its author's
// private set; DemoteToPrivate carries the foreign-reference guard.
//
//	@Summary	Change a provider's scope
//	@Tags		providers
//	@Accept		json
//	@Param		id		path	string		true	"Provider ID"
//	@Param		scope	body	scopeReq	true	"global or private"
//	@Success	204
//	@Failure	400	{object}	ErrorResponse
//	@Failure	409	{object}	ErrorResponse	"name collision in the target scope, or referencing agents block the demote"
//	@Security	BearerAuth
//	@Router		/providers/{id}/scope [post]
func (h *ProviderHandler) SetScope(c *gin.Context) {
	scope, ok := bindScope(c)
	if !ok {
		return
	}
	ctx, id := c.Request.Context(), c.Param("id")
	pv, err := h.store.Get(ctx, id)
	if err != nil {
		storeError(c, err)
		return
	}
	if !scopeChangeAllowed(c, scope, pv.Scope, pv.OwnerID) {
		return
	}
	if sameScope(c, "provider", pv.Scope, scope) {
		return
	}
	if scope == store.ScopePrivate {
		refs, err := h.store.DemoteToPrivate(ctx, id)
		if err != nil {
			saveError(c, err) // name collision in the target scope -> 409
			return
		}
		if refs > 0 {
			conflict(c, fmt.Sprintf("%d agent(s) outside the owner's private set still reference this provider; repoint them first", refs))
			return
		}
		server.SetAuditDetail(c, "scope="+scope)
		c.Status(http.StatusNoContent)
		return
	}
	if err := store.SetScopeOf(ctx, h.store.CrudStore, id, scope, pv.OwnerID); err != nil {
		saveError(c, err) // name collision in the target scope -> 409
		return
	}
	server.SetAuditDetail(c, "scope="+scope)
	c.Status(http.StatusNoContent)
}

// SetOwner transfers the provider, credential included, to another account
// (admin); refused while the move would strand an agent that references it.
//
//	@Summary	Reassign a provider's owner (admin)
//	@Tags		providers
//	@Accept		json
//	@Param		id		path	string			true	"Provider ID"
//	@Param		body	body	SetOwnerRequest	true	"The new owner"
//	@Success	204
//	@Failure	400	{object}	ErrorResponse	"malformed body, or no such user"
//	@Failure	409	{object}	ErrorResponse	"name collision, or referencing agents block the transfer"
//	@Security	BearerAuth
//	@Router		/providers/{id}/owner [put]
func (h *ProviderHandler) SetOwner(c *gin.Context) {
	var req SetOwnerRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.UserID == "" {
		badRequest(c, "user_id is required")
		return
	}
	refs, err := h.store.TransferOwner(c.Request.Context(), c.Param("id"), req.UserID)
	if err != nil {
		if errors.Is(err, store.ErrNoSuchUser) {
			badRequest(c, "no such user")
			return
		}
		saveError(c, err)
		return
	}
	if refs > 0 {
		conflict(c, fmt.Sprintf("%d agent(s) outside the new owner's private set reference this provider; repoint them first", refs))
		return
	}
	server.SetAuditDetail(c, "owner="+req.UserID)
	c.Status(http.StatusNoContent)
}

// Create stores a new provider.
//
//	@Summary	Create a provider
//	@Tags		providers
//	@Accept		json
//	@Produce	json
//	@Param		provider	body		providerReq	true	"Provider"
//	@Success	201			{object}	store.Provider
//	@Failure	400			{object}	ErrorResponse
//	@Failure	409			{object}	ErrorResponse	"duplicate name"
//	@Security	BearerAuth
//	@Router		/providers [post]
func (h *ProviderHandler) Create(c *gin.Context) {
	pv, ok := h.bind(c)
	if !ok {
		return
	}
	if !stampCreateScope(c, &pv.Scope, &pv.OwnerID) {
		return
	}
	// There is no stored value yet, so a mask sentinel resolves to empty.
	pv.APIKey = resolveSecret(pv.APIKey, "")
	if err := h.store.Create(c.Request.Context(), pv); err != nil {
		saveError(c, err) // duplicate name -> 409
		return
	}
	sanitizeProvider(pv)
	created(c, pv.ID, pv)
}

// Update overwrites a provider.
//
//	@Summary		Update a provider
//	@Description	A masked api_key keeps the stored one — but only while the destination (type + base_url) is unchanged, since the stored key belongs to that destination.
//	@Tags			providers
//	@Accept			json
//	@Produce		json
//	@Param			id			path		string		true	"Provider ID"
//	@Param			provider	body		providerReq	true	"Provider"
//	@Success		200			{object}	store.Provider
//	@Failure		400			{object}	ErrorResponse
//	@Failure		404			{object}	ErrorResponse
//	@Failure		409			{object}	ErrorResponse	"duplicate name"
//	@Security		BearerAuth
//	@Router			/providers/{id} [put]
func (h *ProviderHandler) Update(c *gin.Context) {
	pv, ok := h.bind(c)
	if !ok {
		return
	}
	ctx, id := c.Request.Context(), c.Param("id")
	cur, ok := gatedRow(c, h.store.CrudStore, providerScope, editableRow)
	if !ok {
		return
	}
	// The mask resolves against the stored row inside the transaction, only
	// for the destination the key was stored for — invariant 9.
	err := h.store.Update(ctx, id, pv, ownershipGuard(cur.Scope, cur.OwnerID, providerScope,
		func(prev *store.Provider) error {
			pv.Scope, pv.OwnerID = prev.Scope, prev.OwnerID
			if pv.APIKey == SecretMask && prev.APIKey != "" &&
				credentialTargetChanged(prev.Type, prev.BaseURL, pv.Type, pv.BaseURL) {
				return badRequestError("type or base_url changed: the stored api_key belongs to the previous destination — replace it or clear it")
			}
			pv.APIKey = resolveSecret(pv.APIKey, prev.APIKey)
			return nil
		}))
	if err != nil {
		saveError(c, err) // duplicate name -> 409, not-found -> 404
		return
	}
	updated, err := h.store.Get(ctx, id)
	if err != nil {
		storeError(c, err)
		return
	}
	sanitizeProvider(updated)
	c.JSON(http.StatusOK, updated)
}

// Delete removes a provider nothing references.
//
//	@Summary		Delete a provider
//	@Description	Refuses with 409 while an agent still references it — repoint or delete those first.
//	@Tags			providers
//	@Param			id	path	string	true	"Provider ID"
//	@Success		204
//	@Failure		404	{object}	ErrorResponse
//	@Failure		409	{object}	ErrorResponse	"still referenced"
//	@Security		BearerAuth
//	@Router			/providers/{id} [delete]
func (h *ProviderHandler) Delete(c *gin.Context) {
	cur, ok := gatedRow(c, h.store.CrudStore, providerScope, deletableRow)
	if !ok {
		return
	}
	refs, err := h.store.DeleteIfUnreferenced(c.Request.Context(), c.Param("id"), cur.OwnerID)
	if err != nil {
		storeError(c, err)
		return
	}
	if refs > 0 {
		conflict(c, fmt.Sprintf("%d agent(s) still use this provider; repoint them first", refs))
		return
	}
	c.Status(http.StatusNoContent)
}
