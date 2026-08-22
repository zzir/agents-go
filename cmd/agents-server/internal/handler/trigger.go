package handler

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/agents/tasks"
	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// The webhook's signing contract: the caller sends the request's UNIX time and
// HMAC-SHA256(secret, "<timestamp>.<body>") in hex; a call older or newer than
// the tolerance is refused, so a captured request cannot be replayed later.
const (
	HookSignatureHeader = "X-Signature-256"
	HookTimestampHeader = "X-Timestamp"
	HookTimestampSkew   = 5 * time.Minute
	// HookBodyLimit bounds a payload — it lands in a prompt, not a store.
	HookBodyLimit = 64 << 10
)

// TriggerFirer fires triggers and keeps cron ones on the clock; the bridge's
// TriggerScheduler.
type TriggerFirer interface {
	Fire(ctx context.Context, triggerID, payload, source string) (*bridge.Fired, error)
	Sync(ctx context.Context, triggerID string)
}

// TriggerHandler serves the triggers that start work — a workflow, or a turn
// of an agent — without a conversation asking, and the webhook endpoint they
// are called through.
type TriggerHandler struct {
	store    *store.TriggerStore
	sessions *store.SessionStore
	firer    TriggerFirer
	replays  replayGuard
}

// NewTriggerHandler returns a handler over the stores and the firer.
func NewTriggerHandler(s *store.TriggerStore, sessions *store.SessionStore, firer TriggerFirer) *TriggerHandler {
	return &TriggerHandler{store: s, sessions: sessions, firer: firer, replays: replayGuard{seen: map[string]time.Time{}}}
}

// replayGuard remembers each delivery accepted within the timestamp window —
// keyed by trigger and signature, which is the timestamp and body — so the
// same signed request fires once, not once per resend. In memory: the
// window is minutes, and a restart inside it is the one gap left open.
type replayGuard struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// admit reports whether key is new within the window, recording it if so.
func (g *replayGuard) admit(key string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for k, at := range g.seen {
		if now.Sub(at) > 2*HookTimestampSkew {
			delete(g.seen, k)
		}
	}
	if _, dup := g.seen[key]; dup {
		return false
	}
	g.seen[key] = now
	return true
}

// forget releases a key admit recorded: the delivery did not fire, so the
// same signed request may be sent again.
func (g *replayGuard) forget(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.seen, key)
}

// TriggerView is a trigger as the API shows it: the secret only when it was
// just made (creation, rotation), otherwise its last four characters; and the
// path a webhook is called on.
type TriggerView struct {
	*store.Trigger
	// Secret is set on the response that minted it, and never again.
	Secret string `json:"secret,omitempty"`
	// SecretHint is the secret's tail, to tell one from another.
	SecretHint string `json:"secret_hint,omitempty"`
	// HookPath is where a webhook trigger is called (POST, signed).
	HookPath string `json:"hook_path,omitempty"`
}

func viewOf(t *store.Trigger, mintedSecret string) TriggerView {
	v := TriggerView{Trigger: t, Secret: mintedSecret}
	if t.Kind == store.TriggerKindWebhook {
		v.HookPath = HookPath(t.ID)
		if n := len(t.Secret); n >= 4 {
			v.SecretHint = "…" + t.Secret[n-4:]
		}
	}
	return v
}

// HookPath is the webhook URL path of a trigger, relative to the server root.
func HookPath(triggerID string) string { return server.HooksPrefix + "/" + triggerID }

// triggerReq is the request body for Create and Update: what a client may
// set. The id, the secret and the fire record are the server's.
type triggerReq struct {
	Target        string `json:"target,omitempty"`
	WorkflowID    string `json:"workflow_id,omitempty"`
	AgentConfigID string `json:"agent_config_id,omitempty"`
	SessionID     string `json:"session_id"`
	Kind          string `json:"kind"`
	Brief         string `json:"brief"`
	Schedule      string `json:"schedule,omitempty"`
	Enabled       bool   `json:"enabled"`
}

func (r *triggerReq) toModel() *store.Trigger {
	return &store.Trigger{
		Target: r.Target, WorkflowID: r.WorkflowID, AgentConfigID: r.AgentConfigID,
		SessionID: r.SessionID, Kind: r.Kind, Brief: r.Brief, Schedule: r.Schedule, Enabled: r.Enabled,
	}
}

// bind decodes and validates an incoming trigger: its shape, its schedule,
// and the session it names (a conversation, not a task's own). The target —
// the workflow or the agent — is checked by the store, in the transaction
// that writes the row.
func (h *TriggerHandler) bind(c *gin.Context) (*store.Trigger, bool) {
	var req triggerReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err.Error())
		return nil, false
	}
	t := req.toModel()
	if err := store.NormalizeTrigger(t); err != nil {
		badRequest(c, err.Error())
		return nil, false
	}
	if t.Kind == store.TriggerKindCron {
		if err := bridge.ValidateCronSchedule(t.Schedule); err != nil {
			badRequest(c, err.Error())
			return nil, false
		}
	}
	ctx := c.Request.Context()
	sess, err := h.sessions.Get(ctx, t.SessionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			badRequest(c, "session_id names no session")
		} else {
			internalError(c, err)
		}
		return nil, false
	}
	if !ownsSession(c, sess) {
		badRequest(c, "session_id names no session")
		return nil, false
	}
	if sess.Hidden {
		badRequest(c, "session_id names a task's own session; a trigger reports to a conversation")
		return nil, false
	}
	return t, true
}

// List responds with the caller's triggers, or a workflow's with ?workflow_id=.
//
//	@Summary	List triggers
//	@Tags		triggers
//	@Produce	json
//	@Param		workflow_id	query		string	false	"Only this workflow's triggers"
//	@Success	200			{array}		TriggerView
//	@Failure	500			{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/triggers [get]
func (h *TriggerHandler) List(c *gin.Context) {
	u, _ := server.CurrentUser(c)
	rows, err := h.store.ListByOwner(c.Request.Context(), u.ID, c.Query("workflow_id"))
	if err != nil {
		internalError(c, err)
		return
	}
	views := make([]TriggerView, 0, len(rows))
	for i := range rows {
		views = append(views, viewOf(&rows[i], ""))
	}
	c.JSON(http.StatusOK, views)
}

// Get responds with one trigger.
//
//	@Summary	Get a trigger
//	@Tags		triggers
//	@Produce	json
//	@Param		id	path		string	true	"Trigger ID"
//	@Success	200	{object}	TriggerView
//	@Failure	404	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/triggers/{id} [get]
func (h *TriggerHandler) Get(c *gin.Context) {
	t, err := h.store.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		storeError(c, err)
		return
	}
	c.JSON(http.StatusOK, viewOf(t, ""))
}

// Create stores a trigger and puts it on the clock. A webhook trigger's
// secret is minted here and returned ONCE, in this response.
//
//	@Summary		Create a trigger
//	@Description	A cron trigger (schedule: five fields or @hourly/@every 10m) or a webhook trigger, firing its target into the session with the brief: target "workflow" (workflow_id) starts an execution that reports back to the session, target "agent" (agent_config_id) sends the brief as a message of the session, run by that agent. The webhook secret is in this response only; rotate it to get another.
//	@Tags			triggers
//	@Accept			json
//	@Produce		json
//	@Param			trigger	body		triggerReq	true	"Trigger"
//	@Success		201		{object}	TriggerView
//	@Failure		400		{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/triggers [post]
func (h *TriggerHandler) Create(c *gin.Context) {
	t, ok := h.bind(c)
	if !ok {
		return
	}
	minted := ""
	if t.Kind == store.TriggerKindWebhook {
		minted = store.NewTriggerSecret()
		t.Secret = minted
	}
	if err := h.store.Create(c.Request.Context(), t); err != nil {
		h.saveError(c, err)
		return
	}
	h.firer.Sync(c.Request.Context(), t.ID)
	created(c, t.ID, viewOf(t, minted))
}

// saveError maps a trigger write's failure: a reference that vanished between
// the handler's check and the store's is the caller's 400, like the check's.
func (h *TriggerHandler) saveError(c *gin.Context, err error) {
	if errors.Is(err, store.ErrTriggerRef) {
		badRequest(c, err.Error())
		return
	}
	saveError(c, err)
}

// Update overwrites a trigger's settings; the secret and the fire record stay.
//
//	@Summary	Update a trigger
//	@Tags		triggers
//	@Accept		json
//	@Produce	json
//	@Param		id		path		string		true	"Trigger ID"
//	@Param		trigger	body		triggerReq	true	"Trigger"
//	@Success	200		{object}	TriggerView
//	@Failure	400		{object}	ErrorResponse
//	@Failure	404		{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/triggers/{id} [put]
func (h *TriggerHandler) Update(c *gin.Context) {
	ctx, id := c.Request.Context(), c.Param("id")
	cur, err := h.store.Get(ctx, id)
	if err != nil {
		storeError(c, err)
		return
	}
	t, ok := h.bind(c)
	if !ok {
		return
	}
	// The kind is fixed at creation (a webhook's secret and URL would be
	// meaningless on a cron); UpdateSettings touches the settable columns
	// only, so a rotation or a fire racing this update is not undone.
	if t.Kind != cur.Kind {
		badRequest(c, "kind cannot change; create another trigger")
		return
	}
	if err := h.store.UpdateSettings(ctx, id, t); err != nil {
		h.saveError(c, err)
		return
	}
	updated, err := h.store.Get(ctx, id)
	if err != nil {
		storeError(c, err)
		return
	}
	h.firer.Sync(ctx, id)
	c.JSON(http.StatusOK, viewOf(updated, ""))
}

// Delete removes a trigger and takes it off the clock.
//
//	@Summary	Delete a trigger
//	@Tags		triggers
//	@Param		id	path	string	true	"Trigger ID"
//	@Success	204
//	@Failure	404	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/triggers/{id} [delete]
func (h *TriggerHandler) Delete(c *gin.Context) {
	ctx, id := c.Request.Context(), c.Param("id")
	if err := h.store.Delete(ctx, id); err != nil {
		storeError(c, err)
		return
	}
	h.firer.Sync(ctx, id)
	c.Status(http.StatusNoContent)
}

// fireReq is the body of a manual fire: an optional payload, as a webhook
// would carry.
type fireReq struct {
	Payload string `json:"payload"`
}

// Fire starts what the trigger names now, by hand — the way to test one.
//
//	@Summary		Fire a trigger
//	@Description	Starts the trigger's workflow into its session, or its agent's turn in it, as a tick or a webhook call would; the optional payload is appended to the brief. 201 with the task (a workflow) or {run_id} (an agent turn). 400 when the trigger is disabled or the workflow cannot start, 404 for an unknown trigger, 409 when the session is at its background-task cap or busy with a run.
//	@Tags			triggers
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string	true	"Trigger ID"
//	@Param			request	body		fireReq	false	"Optional payload"
//	@Success		201		{object}	bridge.Fired
//	@Failure		400		{object}	ErrorResponse
//	@Failure		404		{object}	ErrorResponse
//	@Failure		409		{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/triggers/{id}/fire [post]
func (h *TriggerHandler) Fire(c *gin.Context) {
	var req fireReq
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			badRequest(c, err.Error())
			return
		}
	}
	h.fire(c, c.Param("id"), req.Payload, bridge.FireManual)
}

// fire is the shared tail of a manual fire and a webhook call; it reports
// whether something was started.
func (h *TriggerHandler) fire(c *gin.Context, id, payload, source string) bool {
	fired, err := h.firer.Fire(c.Request.Context(), id, payload, source)
	if err != nil {
		switch {
		case errors.Is(err, bridge.ErrTriggerDisabled), errors.Is(err, bridge.ErrWorkflowUnavailable), errors.Is(err, bridge.ErrTriggerTarget):
			badRequest(c, err.Error())
		case errors.As(err, new(tasks.ErrTaskLimit)), errors.As(err, new(bridge.ErrTaskLimit)),
			errors.As(err, new(bridge.ErrSessionBusy)), errors.As(err, new(bridge.ErrSessionDeleting)):
			conflict(c, err.Error())
		case errors.As(err, new(bridge.ErrShuttingDown)):
			unavailable(c, err.Error())
		default:
			storeError(c, err)
		}
		return false
	}
	c.JSON(http.StatusCreated, fired)
	return true
}

// RotateSecret mints a webhook trigger a new secret, returned once.
//
//	@Summary		Rotate a webhook trigger's secret
//	@Description	The old secret stops working at once; the new one is in this response only.
//	@Tags			triggers
//	@Produce		json
//	@Param			id	path		string	true	"Trigger ID"
//	@Success		200	{object}	TriggerView
//	@Failure		400	{object}	ErrorResponse	"not a webhook trigger"
//	@Failure		404	{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/triggers/{id}/rotate-secret [post]
func (h *TriggerHandler) RotateSecret(c *gin.Context) {
	ctx, id := c.Request.Context(), c.Param("id")
	t, err := h.store.Get(ctx, id)
	if err != nil {
		storeError(c, err)
		return
	}
	if t.Kind != store.TriggerKindWebhook {
		badRequest(c, "only a webhook trigger has a secret")
		return
	}
	t.Secret = store.NewTriggerSecret()
	if err := h.store.SetSecret(ctx, id, t.Secret); err != nil {
		saveError(c, err)
		return
	}
	c.JSON(http.StatusOK, viewOf(t, t.Secret))
}

// Hook is the webhook endpoint (POST /hooks/{id}), outside the token-guarded
// API and so outside the OpenAPI document's server base — the README is its
// contract. The caller proves itself with the trigger's secret: X-Timestamp
// (UNIX seconds, within HookTimestampSkew) and X-Signature-256 =
// hex(HMAC-SHA256(secret, timestamp + "." + body)); the body, up to
// HookBodyLimit, is the payload appended to the brief. 401 on a bad or stale
// signature; otherwise as a manual fire.
func (h *TriggerHandler) Hook(c *gin.Context) {
	ctx, id := c.Request.Context(), c.Param("id")
	t, err := h.store.Get(ctx, id)
	if err != nil {
		storeError(c, err)
		return
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, HookBodyLimit+1))
	if err != nil {
		badRequest(c, "reading the body: "+err.Error())
		return
	}
	if len(body) > HookBodyLimit {
		badRequest(c, "payload larger than 64 KB")
		return
	}
	now := time.Now()
	sig := c.GetHeader(HookSignatureHeader)
	if t.Kind != store.TriggerKindWebhook || !VerifyHookSignature(t.Secret, c.GetHeader(HookTimestampHeader), sig, body, now) {
		abortError(c, http.StatusUnauthorized, protocol.CodeUnauthorized, "bad or stale signature")
		return
	}
	// A valid delivery fires once: the same timestamp and body resent — by a
	// retrying sender or a captured request — is a replay, refused. Claimed
	// before the fire (two copies at once must not both start something) and
	// released when the fire fails, so a sender's resend of a delivery that
	// found the session busy or the server draining still gets through.
	key := id + ":" + strings.ToLower(strings.TrimPrefix(strings.TrimSpace(sig), "sha256="))
	if !h.replays.admit(key, now) {
		conflict(c, "replayed delivery: this timestamp and body were already accepted or are being delivered")
		return
	}
	if !h.fire(c, id, string(body), bridge.FireWebhook) {
		h.replays.forget(key)
	}
}

// SignHook computes the signature a caller sends: hex(HMAC-SHA256(secret,
// timestamp + "." + body)). Exported for clients and tests.
func SignHook(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyHookSignature checks a call against the trigger's secret: the
// timestamp within HookTimestampSkew of now, the signature equal (in constant
// time) to SignHook's over the same timestamp and body. A "sha256=" prefix on
// the signature is accepted.
func VerifyHookSignature(secret, timestamp, signature string, body []byte, now time.Time) bool {
	if secret == "" || timestamp == "" || signature == "" {
		return false
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(timestamp), 10, 64)
	if err != nil {
		return false
	}
	if d := now.Sub(time.Unix(ts, 0)); d > HookTimestampSkew || d < -HookTimestampSkew {
		return false
	}
	want := SignHook(secret, strings.TrimSpace(timestamp), body)
	got := strings.TrimPrefix(strings.TrimSpace(signature), "sha256=")
	return hmac.Equal([]byte(strings.ToLower(got)), []byte(want))
}
