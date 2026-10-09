package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
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

// SettingView is a stored setting as the API returns it: the value (masked for
// secrets) and whether the registry still names the key.
type SettingView struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	// Unknown marks a row the registry does not define (a removed feature's).
	// Reads list it so it can be seen and deleted; writes refuse it.
	Unknown bool `json:"unknown,omitempty"`
}

func settingViewOf(st store.Setting) SettingView {
	v := SettingView{Key: st.Key, Value: st.Value}
	if _, known := settings.Lookup(st.Key); !known {
		// Whether an unknown row was a secret is unknowable: mask it.
		v.Unknown = true
		v.Value = maskSecret(v.Value)
		return v
	}
	if settings.IsSecret(st.Key) {
		v.Value = maskSecret(v.Value)
	}
	return v
}

// storageReadable reports whether the caller may read key: the storage group
// is the admin's, the rest everyone's.
func storageReadable(c *gin.Context, key string) bool {
	if !settings.IsS3Key(key) {
		return true
	}
	u, _ := server.CurrentUser(c)
	return u.Role == store.RoleAdmin
}

// List responds with all stored settings, secrets masked.
//
//	@Summary		List settings
//	@Description	Every stored key/value. Secrets are masked; the storage (s3_*) keys are listed for admins only. A key the registry no longer defines is flagged `unknown` with its value masked too (whether it was a secret is unknowable), so it can be deleted. The definitions themselves are at /setting-defs.
//	@Tags			settings
//	@Produce		json
//	@Success		200	{array}		SettingView
//	@Failure		500	{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/settings [get]
func (h *SettingHandler) List(c *gin.Context) {
	stored, err := h.store.List(c.Request.Context())
	if err != nil {
		internalError(c, err)
		return
	}
	out := make([]SettingView, 0, len(stored))
	for _, st := range stored {
		if storageReadable(c, st.Key) {
			out = append(out, settingViewOf(st))
		}
	}
	c.JSON(http.StatusOK, out)
}

// Get responds with the setting identified by the key path parameter, secrets
// masked.
//
//	@Summary		Get setting
//	@Description	Secrets are masked; a storage (s3_*) key is 403 for a member.
//	@Tags			settings
//	@Produce		json
//	@Param			key	path		string	true	"Setting key"
//	@Success		200	{object}	SettingView
//	@Failure		403	{object}	ErrorResponse
//	@Failure		404	{object}	ErrorResponse
//	@Failure		500	{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/settings/{key} [get]
func (h *SettingHandler) Get(c *gin.Context) {
	if !storageReadable(c, c.Param("key")) {
		requireAdmin(c)
		return
	}
	st, err := h.store.Get(c.Request.Context(), c.Param("key"))
	if err != nil {
		storeError(c, err)
		return
	}
	c.JSON(http.StatusOK, settingViewOf(*st))
}

type setSettingReq struct {
	Value string `json:"value"`
}

// Set writes the value for the key path parameter and responds with the stored
// setting; a masked value keeps a stored secret.
//
//	@Summary		Set setting
//	@Description	The key must be one the registry defines (see /setting-defs) and the value must suit its kind; either failure is a 400. An empty value returns the setting to its default.
//	@Tags			settings
//	@Accept			json
//	@Produce		json
//	@Param			key		path		string			true	"Setting key"
//	@Param			setting	body		setSettingReq	true	"Value to store"
//	@Success		200		{object}	SettingView
//	@Failure		400		{object}	ErrorResponse
//	@Failure		500		{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/settings/{key} [put]
func (h *SettingHandler) Set(c *gin.Context) {
	var req setSettingReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err.Error())
		return
	}
	ctx := c.Request.Context()
	key := c.Param("key")
	// The attachment-storage section saves as ONE group — workbench invariant 58.
	if settings.IsS3Key(key) {
		badRequest(c, "attachment-storage keys are saved together — use the Attachment storage form (PUT /attachments/storage)")
		return
	}
	// A client echoing the mask keeps the stored secret, resolved inside the
	// transaction; validated AFTER the mask resolves.
	err := h.store.Modify(ctx, key, func(prev string, found bool) (string, error) {
		if settings.IsSecret(key) && req.Value == SecretMask {
			req.Value = ""
			if found {
				req.Value = prev
			}
		}
		if err := settings.Validate(key, req.Value); err != nil {
			return "", badRequestError(err.Error())
		}
		return req.Value, nil
	})
	if err != nil {
		saveError(c, err)
		return
	}
	c.JSON(http.StatusOK, settingViewOf(store.Setting{Key: key, Value: req.Value}))
}

// Delete removes the setting identified by the key path parameter; unvalidated,
// so an unknown key can be cleared.
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
	key := c.Param("key")
	if settings.IsS3Key(key) {
		badRequest(c, "attachment-storage keys are cleared together — use the Attachment storage form's Clear")
		return
	}
	if err := h.store.Delete(c.Request.Context(), key); err != nil {
		storeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// SettingDefList responds with the setting registry: every key, its kind,
// default and presentation (invariant 40).
//
//	@Summary	List setting definitions
//	@Tags		settings
//	@Produce	json
//	@Success	200	{array}	settings.Def
//	@Security	BearerAuth
//	@Router		/setting-defs [get]
func SettingDefList(c *gin.Context) {
	c.JSON(http.StatusOK, settings.Defs())
}
