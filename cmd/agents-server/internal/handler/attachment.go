package handler

import (
	"bytes"
	"context"
	"image"
	_ "image/jpeg" // DecodeConfig support for the two admitted formats
	_ "image/png"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/attachments"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// MaxAttachmentBytes aliases the attachments-package limit for the wire-up
// in cmd (body cap for the upload route).
const MaxAttachmentBytes = attachments.MaxBytes

// AttachmentHandler serves image-attachment upload, revocation, the
// client-facing feature config, and the storage section's form endpoints.
type AttachmentHandler struct {
	store        *store.AttachmentStore
	settings     *settings.Reader
	settingStore *store.SettingStore
	// OnStorageChange, when set, fires after the storage section changes —
	// the server refreshes the CSP img-src with the new public host, so a
	// changed bucket shows images without a restart.
	OnStorageChange func()
}

// NewAttachmentHandler returns a handler over the given stores and settings.
func NewAttachmentHandler(s *store.AttachmentStore, cfg *settings.Reader, settingStore *store.SettingStore) *AttachmentHandler {
	return &AttachmentHandler{store: s, settings: cfg, settingStore: settingStore}
}

// client returns the bucket client for the current settings, or nil when the
// feature is unconfigured.
func (h *AttachmentHandler) client(ctx context.Context) *attachments.Client {
	return attachments.ClientFrom(h.settings.S3Config(ctx), h.settings.ProxyClient(ctx))
}

// attachmentConfigResp is what the composer renders and validates from.
type attachmentConfigResp struct {
	// Enabled reports whether attachment storage is configured; off hides the
	// upload affordances entirely.
	Enabled     bool  `json:"enabled"`
	MaxBytes    int64 `json:"max_bytes"`
	MaxCount    int   `json:"max_count"`
	DownscalePx int   `json:"downscale_px"`
}

// Config responds with the attachment feature's availability and limits.
//
//	@Summary		Attachment config
//	@Description	Whether image attachments are configured (the Attachment storage settings are complete) and the limits the client should apply: max bytes per image, max images per message, and the longest-side pixel target to downscale to before uploading.
//	@Tags			attachments
//	@Produce		json
//	@Success		200	{object}	attachmentConfigResp
//	@Security		BearerAuth
//	@Router			/attachments/config [get]
func (h *AttachmentHandler) Config(c *gin.Context) {
	c.JSON(http.StatusOK, attachmentConfigResp{
		Enabled:     h.settings.S3Config(c.Request.Context()).Complete(),
		MaxBytes:    attachments.MaxBytes,
		MaxCount:    attachments.MaxPerMessage,
		DownscalePx: attachments.DownscalePx,
	})
}

// attachmentResp is one stored attachment as the API returns it.
type attachmentResp struct {
	ID   string `json:"id"`
	URL  string `json:"url"`
	Mime string `json:"mime"`
	Size int64  `json:"size"`
}

func (h *AttachmentHandler) resp(ctx context.Context, a *store.Attachment) attachmentResp {
	base := h.settings.S3Config(ctx).PublicBaseURL
	return attachmentResp{
		ID: a.ID, URL: attachments.PublicURL(base, a.Key),
		Mime: a.Mime, Size: a.Size,
	}
}

// Upload stores one image: bytes to the bucket, metadata as a row. The image
// is validated by decoding, never by trusting the request's content type.
//
//	@Summary		Upload attachment
//	@Description	Multipart upload of one image (field "file", png or jpeg, max 10 MiB, longest side max 8000px). The bytes go to the configured bucket under a public URL; the response carries the id to pass as attachment_ids when starting a run. An upload never sent with a message is garbage-collected after 24h. 503 when attachment storage is not configured.
//	@Tags			attachments
//	@Accept			mpfd
//	@Produce		json
//	@Param			file	formData	file	true	"Image file (png/jpeg)"
//	@Success		201		{object}	attachmentResp
//	@Failure		400		{object}	ErrorResponse
//	@Failure		503		{object}	ErrorResponse	"attachment storage not configured"
//	@Security		BearerAuth
//	@Router			/attachments [post]
func (h *AttachmentHandler) Upload(c *gin.Context) {
	u, ok := server.CurrentUser(c)
	if !ok {
		abortError(c, http.StatusUnauthorized, protocol.CodeUnauthorized, "no user")
		return
	}
	ctx := c.Request.Context()
	client := h.client(ctx)
	if client == nil {
		abortError(c, http.StatusServiceUnavailable, protocol.CodeUnavailable,
			"image attachments are not configured — an admin must fill the Attachment storage settings")
		return
	}
	// The request body is capped by the route's limit (SetBodyLimit in the
	// composition root: MaxAttachmentBytes plus multipart slack).
	file, _, err := c.Request.FormFile("file")
	if err != nil {
		badRequest(c, "multipart field \"file\" is required (max 10 MiB)")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, attachments.MaxBytes+1))
	if err != nil {
		badRequest(c, "reading upload: "+err.Error())
		return
	}
	if int64(len(data)) > attachments.MaxBytes {
		badRequest(c, "image exceeds the 10 MiB limit")
		return
	}

	// The decode is the validation: sniffed MIME fast-rejects, DecodeConfig
	// proves the bytes are the image they claim and yields the dimensions.
	mime := http.DetectContentType(data)
	if mime != "image/png" && mime != "image/jpeg" {
		badRequest(c, "only png and jpeg images are accepted, got "+mime)
		return
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "png" && format != "jpeg") {
		badRequest(c, "file does not decode as a png or jpeg image")
		return
	}
	if cfg.Width > attachments.MaxSidePx || cfg.Height > attachments.MaxSidePx {
		badRequest(c, "image dimensions exceed 8000px — downscale before uploading")
		return
	}

	// Key layout: attachments/{owner}/{uuid}.{ext} — the extension derives
	// from the DECODED format (always lowercase), never the client's filename.
	ext := ".png"
	if format == "jpeg" {
		ext = ".jpg"
	}
	a := &store.Attachment{
		OwnerID: u.ID,
		Key:     "attachments/" + u.ID + "/" + store.NewID() + ext,
		Mime:    mime,
		Size:    int64(len(data)),
	}
	if err := client.Put(ctx, a.Key, mime, data); err != nil {
		abortError(c, http.StatusBadGateway, protocol.CodeUpstream, err.Error())
		return
	}
	if err := h.store.Create(ctx, a); err != nil {
		// The object is orphaned in the bucket; remove it best-effort rather
		// than leaving an unreferenced key forever.
		delCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = client.Delete(delCtx, a.Key)
		internalError(c, err)
		return
	}
	c.JSON(http.StatusCreated, h.resp(ctx, a))
}

// Delete revokes an attachment that was never sent. Owner-only; someone
// else's id reads as absent. A bound attachment is part of session history
// and refuses deletion.
//
//	@Summary		Delete attachment
//	@Description	Removes an uploaded image that has not been sent with a message yet (the composer's ✕). One already accepted by a run is part of session history and answers 409.
//	@Tags			attachments
//	@Param			id	path	string	true	"Attachment ID"
//	@Success		204	"deleted"
//	@Failure		404	{object}	ErrorResponse
//	@Failure		409	{object}	ErrorResponse	"already sent with a message"
//	@Security		BearerAuth
//	@Router			/attachments/{id} [delete]
func (h *AttachmentHandler) Delete(c *gin.Context) {
	u, ok := server.CurrentUser(c)
	if !ok {
		abortError(c, http.StatusUnauthorized, protocol.CodeUnauthorized, "no user")
		return
	}
	ctx := c.Request.Context()
	a, err := h.store.Get(ctx, c.Param("id"))
	if err != nil {
		storeError(c, err)
		return
	}
	if a.OwnerID != u.ID {
		notFound(c) // ownership is not an oracle for existence
		return
	}
	if a.Bound {
		abortError(c, http.StatusConflict, protocol.CodeConflict, "attachment was sent with a message and is part of session history")
		return
	}
	// Object first, row second — the same order the reaper uses: a row whose
	// object delete failed is retried, a dangling sentinel is forever.
	if client := h.client(ctx); client != nil {
		if err := client.Delete(ctx, a.Key); err != nil {
			abortError(c, http.StatusBadGateway, protocol.CodeUpstream, err.Error())
			return
		}
	}
	if err := h.store.Delete(ctx, a.ID); err != nil {
		internalError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// storageReq is the attachment-storage section as ONE value — the form's
// shape. The section's keys are only valid together (changing the bucket
// re-validates against the same public base), which is why they save as a
// group instead of key by key.
type storageReq struct {
	Endpoint        string `json:"endpoint"`
	Region          string `json:"region"`
	Bucket          string `json:"bucket"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	PublicBaseURL   string `json:"public_base_url"`
	PathStyle       bool   `json:"path_style"`
}

// resolve turns the request into the S3Config it describes: fields trimmed,
// the masked secret replaced by the stored one, an empty region defaulted.
func (h *AttachmentHandler) resolve(ctx context.Context, req storageReq) settings.S3Config {
	secret := strings.TrimSpace(req.SecretAccessKey)
	if secret == SecretMask {
		secret = h.settings.S3Config(ctx).SecretKey
	}
	region := strings.TrimSpace(req.Region)
	if region == "" {
		d, _ := settings.Lookup(settings.KeyS3Region)
		region = d.Default
	}
	return settings.S3Config{
		Endpoint:      strings.TrimSpace(req.Endpoint),
		Region:        region,
		Bucket:        strings.TrimSpace(req.Bucket),
		AccessKeyID:   strings.TrimSpace(req.AccessKeyID),
		SecretKey:     secret,
		PublicBaseURL: strings.TrimSpace(req.PublicBaseURL),
		PathStyle:     req.PathStyle,
	}
}

// checkStorage validates a non-empty section: every required field present,
// the URLs well-formed, and the bucket probed end to end (signed upload,
// anonymous public read, delete). Returns the message for a 400, "" when the
// section is usable.
func (h *AttachmentHandler) checkStorage(ctx context.Context, cfg settings.S3Config) string {
	if !cfg.Complete() {
		return "all fields are required (region is optional and defaults to auto)"
	}
	for key, v := range map[string]string{settings.KeyS3Endpoint: cfg.Endpoint, settings.KeyS3PublicBaseURL: cfg.PublicBaseURL} {
		if err := settings.Validate(key, v); err != nil {
			return err.Error()
		}
	}
	client := attachments.ClientFrom(cfg, h.settings.ProxyClient(ctx))
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := client.Probe(probeCtx); err != nil {
		return err.Error()
	}
	return ""
}

// isStorageClear reports an all-empty submission — the form's Clear.
func (req storageReq) isStorageClear() bool {
	return strings.TrimSpace(req.Endpoint) == "" && strings.TrimSpace(req.Bucket) == "" &&
		strings.TrimSpace(req.AccessKeyID) == "" && strings.TrimSpace(req.SecretAccessKey) == "" &&
		strings.TrimSpace(req.PublicBaseURL) == ""
}

// SaveStorage writes the attachment-storage section as one group: probed
// before anything lands, stored in one transaction, and an all-empty body
// clears the section (turning the feature off).
//
//	@Summary		Save attachment storage
//	@Description	Saves the whole attachment-storage section atomically. A non-empty section is probed end to end first (signed upload, anonymous public read, delete) and refused with 400 if any stage fails — so changing one field is validated against the section it will actually be stored with. An all-empty body clears the section and turns image input off. A masked secret_access_key ("********") keeps the stored secret.
//	@Tags			attachments
//	@Accept			json
//	@Produce		json
//	@Param			storage	body		storageReq	true	"The whole section"
//	@Success		200		{object}	attachmentConfigResp
//	@Failure		400		{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/attachments/storage [put]
func (h *AttachmentHandler) SaveStorage(c *gin.Context) {
	var req storageReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err.Error())
		return
	}
	ctx := c.Request.Context()
	kv := map[string]string{
		settings.KeyS3Endpoint: "", settings.KeyS3Region: "", settings.KeyS3Bucket: "",
		settings.KeyS3AccessKeyID: "", settings.KeyS3SecretAccessKey: "",
		settings.KeyS3PublicBaseURL: "", settings.KeyS3PathStyle: "",
	}
	if !req.isStorageClear() {
		cfg := h.resolve(ctx, req)
		if msg := h.checkStorage(ctx, cfg); msg != "" {
			badRequest(c, msg)
			return
		}
		kv[settings.KeyS3Endpoint] = cfg.Endpoint
		kv[settings.KeyS3Region] = cfg.Region
		kv[settings.KeyS3Bucket] = cfg.Bucket
		kv[settings.KeyS3AccessKeyID] = cfg.AccessKeyID
		kv[settings.KeyS3SecretAccessKey] = cfg.SecretKey
		kv[settings.KeyS3PublicBaseURL] = cfg.PublicBaseURL
		kv[settings.KeyS3PathStyle] = strconv.FormatBool(cfg.PathStyle)
	}
	if err := h.settingStore.SetMany(ctx, kv); err != nil {
		internalError(c, err)
		return
	}
	if h.OnStorageChange != nil {
		h.OnStorageChange()
	}
	h.Config(c)
}

// TestStorage runs the end-to-end probe against the SUBMITTED values without
// storing anything — the form's Test button.
//
//	@Summary		Test attachment storage
//	@Description	Probes the submitted section (signed upload, anonymous public read through the public base URL, delete) without storing it. 200 when the bucket is usable; 400 with the failing stage otherwise. A masked secret_access_key uses the stored secret.
//	@Tags			attachments
//	@Accept			json
//	@Success		204	"bucket verified"
//	@Failure		400	{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/attachments/storage/test [post]
func (h *AttachmentHandler) TestStorage(c *gin.Context) {
	var req storageReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err.Error())
		return
	}
	ctx := c.Request.Context()
	if msg := h.checkStorage(ctx, h.resolve(ctx, req)); msg != "" {
		badRequest(c, msg)
		return
	}
	c.Status(http.StatusNoContent)
}
