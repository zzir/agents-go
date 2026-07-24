package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// SettingHandler serves endpoints for reading and writing key-value settings.
type SettingHandler struct {
	store *store.SettingStore
}

// NewSettingHandler returns a handler backed by the given store.
func NewSettingHandler(s *store.SettingStore) *SettingHandler {
	return &SettingHandler{store: s}
}

// List responds with all settings, secret values masked.
//
//	@Summary		List settings
//	@Description	Known keys: proxy_url, system_prompt, brave_api_key (secret, masked).
//	@Tags			settings
//	@Produce		json
//	@Success		200	{array}		store.Setting
//	@Failure		500	{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/settings [get]
func (h *SettingHandler) List(c *gin.Context) {
	settings, err := h.store.List(c.Request.Context())
	if err != nil {
		internalError(c, err)
		return
	}
	for i := range settings {
		if secretSettingKeys[settings[i].Key] {
			settings[i].Value = maskSecret(settings[i].Value)
		}
	}
	c.JSON(http.StatusOK, settings)
}

// Get responds with the setting identified by the key path parameter, secret
// values masked.
//
//	@Summary	Get setting
//	@Tags		settings
//	@Produce	json
//	@Param		key	path		string	true	"Setting key"
//	@Success	200	{object}	store.Setting
//	@Failure	404	{object}	ErrorResponse
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/settings/{key} [get]
func (h *SettingHandler) Get(c *gin.Context) {
	st, err := h.store.Get(c.Request.Context(), c.Param("key"))
	if err != nil {
		storeError(c, err)
		return
	}
	if secretSettingKeys[st.Key] {
		st.Value = maskSecret(st.Value)
	}
	c.JSON(http.StatusOK, st)
}

type setSettingReq struct {
	Value string `json:"value"`
}

// Set writes the value for the setting identified by the key path parameter
// and responds with the stored setting (secret values masked). For secret
// settings, a masked value keeps the stored one.
//
//	@Summary	Set setting
//	@Tags		settings
//	@Accept		json
//	@Produce	json
//	@Param		key		path		string			true	"Setting key"
//	@Param		setting	body		setSettingReq	true	"Value to store"
//	@Success	200		{object}	store.Setting
//	@Failure	400		{object}	ErrorResponse
//	@Failure	500		{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/settings/{key} [put]
func (h *SettingHandler) Set(c *gin.Context) {
	var req setSettingReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err.Error())
		return
	}
	ctx := c.Request.Context()
	key := c.Param("key")
	if secretSettingKeys[key] && req.Value == SecretMask {
		// Keep the stored secret when the client echoes the mask. A transient
		// (non-not-found) Get failure must abort: continuing would resolve the mask
		// to "" and silently clear the stored secret. Not-found leaves nothing to
		// preserve, so the mask resolves to "".
		prev, err := h.store.Get(ctx, key)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			internalError(c, err)
			return
		}
		req.Value = ""
		if prev != nil {
			req.Value = prev.Value
		}
	}
	if err := h.store.Set(ctx, key, req.Value); err != nil {
		internalError(c, err)
		return
	}
	st := store.Setting{Key: key, Value: req.Value}
	if secretSettingKeys[key] {
		st.Value = maskSecret(st.Value)
	}
	c.JSON(http.StatusOK, st)
}

// Delete removes the setting identified by the key path parameter.
//
//	@Summary	Delete setting
//	@Tags		settings
//	@Param		key	path	string	true	"Setting key"
//	@Success	204	"deleted"
//	@Failure	404	{object}	ErrorResponse
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/settings/{key} [delete]
func (h *SettingHandler) Delete(c *gin.Context) {
	if err := h.store.Delete(c.Request.Context(), c.Param("key")); err != nil {
		storeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
