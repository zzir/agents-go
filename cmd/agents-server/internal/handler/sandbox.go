package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/sandboxes"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	dockersb "github.com/zzir/agents-go/sandbox/docker"
)

// SandboxHandler serves CRUD endpoints, the health check and the docker
// operator surface for sandboxes — the machines projects run on and the images
// they run (decisions §5.36).
type SandboxHandler struct {
	store   *store.SandboxStore
	manager *sandboxes.Manager
	retire  *Retirer
}

// NewSandboxHandler returns a handler over the sandbox store; the manager runs
// the health check, and the retirer is what carries a content change to the
// projects on this sandbox.
func NewSandboxHandler(s *store.SandboxStore, m *sandboxes.Manager, r *Retirer) *SandboxHandler {
	if s == nil || m == nil || r == nil {
		panic("handler: NewSandboxHandler needs the sandbox store, the manager and a retirer")
	}
	return &SandboxHandler{store: s, manager: m, retire: r}
}

// Retirer turns a content change on a sandbox into the project generations it
// invalidates, then retires each project's live instance and terminals. It is
// the one place the "one runtime axis" rule is applied (decisions §5.33).
type Retirer struct {
	projects  *store.ProjectStore
	manager   *sandboxes.Manager
	terminals *TerminalHandler
}

// NewRetirer wires the three things a retirement touches.
func NewRetirer(projects *store.ProjectStore, m *sandboxes.Manager, terminals *TerminalHandler) *Retirer {
	if projects == nil || terminals == nil {
		panic("handler: NewRetirer needs the project store and the terminal handler")
	}
	return &Retirer{projects: projects, manager: m, terminals: terminals}
}

// bump moves the runtime generation of every project on the sandbox and
// retires what each was serving. A failure is returned to the caller, which
// answers 500: the row is already written, and silently leaving live
// containers on the old configuration is the worse outcome to hide.
func (r *Retirer) bump(ctx context.Context, sandboxID string) error {
	gens, err := r.projects.BumpRuntimeGen(ctx, sandboxID)
	if err != nil {
		return err
	}
	for _, g := range gens {
		if r.manager != nil {
			r.manager.RetireProject(g.ID, g.RuntimeGen)
		}
		r.terminals.CloseProjectTerminals(g.ID, g.RuntimeGen)
	}
	return nil
}

// List responds with all sandboxes, secrets masked.
//
//	@Summary	List sandboxes
//	@Tags		sandboxes
//	@Produce	json
//	@Success	200	{array}		store.Sandbox
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/sandboxes [get]
func (h *SandboxHandler) List(c *gin.Context) {
	rows, err := h.store.List(c.Request.Context())
	if err != nil {
		internalError(c, err)
		return
	}
	for i := range rows {
		rows[i] = sanitizeSandboxConfig(rows[i])
	}
	c.JSON(http.StatusOK, rows)
}

type sandboxReq struct {
	Name   string          `json:"name"`
	Type   string          `json:"type"`
	Config json.RawMessage `json:"config"`
	// Revision, on update only, is the revision the client's edit was based
	// on (from GET/List): editing from a stale form is then 409 instead of
	// silently overwriting a concurrent update. Absent (0) keeps
	// last-writer-wins. Ignored on create.
	Revision int64 `json:"revision,omitempty"`
}

func (r sandboxReq) toSandbox() *store.Sandbox {
	return &store.Sandbox{Name: r.Name, Type: r.Type, Config: r.Config}
}

// validate enforces the POLICY layer of a sandbox write: a name, and a type
// this build carries. Field-level validation and canonicalization live in
// store.NormalizeSandboxConfig, which both write handlers run right after
// this.
func (h *SandboxHandler) validate(c *gin.Context, req *sandboxReq) bool {
	if req.Name == "" {
		badRequest(c, "name is required")
		return false
	}
	if req.Type == "" {
		badRequest(c, "type is required")
		return false
	}
	if !slices.Contains(store.SandboxTypes, req.Type) {
		badRequest(c, "type must be one of "+strings.Join(store.SandboxTypes, ", ")+", got "+req.Type)
		return false
	}
	return true
}

// Create persists a new sandbox from the request body.
//
//	@Summary		Create sandbox
//	@Description	type "docker" config: host ("" = local daemon, tcp://, or ssh://user@host with ssh_* auth), image (required), runtime, user ("" = root), network (docker network name; "" = no network), memory_mb/cpus caps, max_read_file_bytes. type "e2b" config: api_url, domain, api_key, data_plane_auth, template_id (required — build it on the service first), timeout_seconds, auto_pause, allow_internet, max_read_file_bytes. ssh_password and api_key are write-only, ******** mask semantics.
//	@Tags			sandboxes
//	@Accept			json
//	@Produce		json
//	@Param			sandbox	body		sandboxReq	true	"Sandbox"
//	@Success		201		{object}	store.Sandbox
//	@Failure		400		{object}	ErrorResponse
//	@Failure		500		{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/sandboxes [post]
func (h *SandboxHandler) Create(c *gin.Context) {
	var req sandboxReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err.Error())
		return
	}
	if !h.validate(c, &req) {
		return
	}
	sb := req.toSandbox()
	// No stored config yet: mask sentinels resolve to empty.
	sb.Config = restoreSandboxConfig(sb.Config, nil)
	canonical, err := store.NormalizeSandboxConfig(sb.Type, sb.Config)
	if err != nil {
		badRequest(c, err.Error())
		return
	}
	sb.Config = canonical
	if err := h.store.Create(c.Request.Context(), sb); err != nil {
		internalError(c, err)
		return
	}
	view := sanitizeSandboxConfig(*sb)
	created(c, view.ID, view)
}

// Get responds with one sandbox, secrets masked.
//
//	@Summary	Get sandbox
//	@Tags		sandboxes
//	@Produce	json
//	@Param		id	path		string	true	"Sandbox id"
//	@Success	200	{object}	store.Sandbox
//	@Failure	404	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/sandboxes/{id} [get]
func (h *SandboxHandler) Get(c *gin.Context) {
	sb, err := h.store.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		storeError(c, err)
		return
	}
	c.JSON(http.StatusOK, sanitizeSandboxConfig(*sb))
}

// Update overwrites the sandbox and responds with the updated row. A masked
// credential keeps the stored value. An update that would change the
// sandbox's identity (type, daemon host or service URL — see
// SandboxIdentityChanged) is refused with 409 while any project lives on it:
// a project's files are at that address, and rewriting what the id points at
// would move every one of them. The image and the limits update freely, and
// reach bound sessions at their next run.
//
//	@Summary		Update sandbox
//	@Description	Include the revision the edit was based on (from GET/List) to make the write conditional: 409 if the row changed meanwhile. Omitting it falls back to last-writer-wins.
//	@Tags			sandboxes
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string		true	"Sandbox id"
//	@Param			sandbox	body		sandboxReq	true	"Sandbox"
//	@Success		200		{object}	store.Sandbox
//	@Failure		400		{object}	ErrorResponse
//	@Failure		404		{object}	ErrorResponse
//	@Failure		409		{object}	ErrorResponse	"identity change refused (projects live on it), or the row changed concurrently — re-read and retry"
//	@Failure		500		{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/sandboxes/{id} [put]
func (h *SandboxHandler) Update(c *gin.Context) {
	var req sandboxReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err.Error())
		return
	}
	if !h.validate(c, &req) {
		return
	}
	id := c.Param("id")
	ctx := c.Request.Context()
	// Load the current row so masked secrets can round-trip to their stored
	// values, and so its revision anchors the CAS below. A transient
	// (non-not-found) Get failure must abort: continuing with an empty prev
	// would resolve the ******** mask to "" and silently WIPE the stored
	// credential — the same guard every sibling handler carries.
	prev, err := h.store.Get(ctx, id)
	if err != nil {
		storeError(c, err)
		return
	}
	sb := req.toSandbox()
	// A masked credential means "keep the stored one", which only holds while
	// the destination is unchanged: moving the machine or the service must not
	// carry the old key along. Only refuse when one is actually stored — a
	// mask with nothing behind it resolves to "" and needs no guard.
	if strings.Contains(string(sb.Config), SecretMask) && storedSandboxSecret(prev.Config) &&
		store.SandboxDestinationChanged(prev.Type, prev.Config, sb.Config) {
		badRequest(c, "the destination changed: the stored credential belongs to the previous one — replace it or clear it")
		return
	}
	sb.Config = restoreSandboxConfig(sb.Config, prev.Config)
	// Normalize AFTER the mask restore: the canonical form must carry the
	// real secret, not the ******** sentinel.
	canonical, err := store.NormalizeSandboxConfig(sb.Type, sb.Config)
	if err != nil {
		badRequest(c, err.Error())
		return
	}
	sb.Config = canonical
	// Everything decided from prev — the identity comparison, contentChanged
	// — holds only while the row IS prev, which the expected-revision CAS on
	// both write paths guarantees: a concurrent update moves the revision,
	// this write refuses (409), the client re-reads. The anchor is the
	// client's own revision when the request names one, extending the
	// guarantee back to the form the edit was made on.
	expected := prev.Revision
	if req.Revision != 0 {
		expected = req.Revision
	}
	contentChanged := prev.Type != sb.Type || !store.SandboxContentEqual(sb.Type, prev.Config, sb.Config)
	if store.SandboxIdentityChanged(prev, sb) {
		projects, uerr := h.store.UpdateIdentityIfUnreferenced(ctx, id, sb, expected)
		if uerr != nil {
			storeError(c, uerr)
			return
		}
		if projects > 0 {
			frozen := "its type and machine are frozen — the image, the limits, the credential and the name stay editable"
			if prev.Type == "e2b" {
				frozen = "its type, service address, template and lifecycle (auto-pause, internet) are frozen — the api key, timeout, read limit and name stay editable"
			}
			conflict(c, fmt.Sprintf("%d project(s) live on this sandbox; %s, or create a new sandbox for the new location", projects, frozen))
			return
		}
	} else if err := h.store.Update(ctx, id, sb, expected); err != nil {
		storeError(c, err)
		return
	}
	// The write landed; invalidate NOW, from what the CAS guarantees — not
	// from a re-read that a cancelled request could fail, leaving a new image
	// in the store while old instances and terminals keep serving. Only a
	// CONTENT change retires: a rename must not sever terminals or close idle
	// containers.
	if contentChanged {
		// WithoutCancel: the row is written; a disconnect here must not skip
		// the generation bump, or live instances and terminals keep serving
		// the replaced image/credential with no path retiring them.
		if err := h.retire.bump(context.WithoutCancel(ctx), id); err != nil {
			internalError(c, err)
			return
		}
	}
	updated, err := h.store.Get(ctx, id)
	if err != nil {
		storeError(c, err)
		return
	}
	c.JSON(http.StatusOK, sanitizeSandboxConfig(*updated))
}

// Delete removes the sandbox. One still carrying projects is refused with
// 409: their working trees live on that machine, and the operator deletes them
// (which reclaims their storage) first. The refusal is decided by the delete
// statement itself, not a prior count — a project create racing this delete
// therefore either lands before it (and blocks it) or loses its own lock.
//
//	@Summary	Delete sandbox
//	@Tags		sandboxes
//	@Param		id	path	string	true	"Sandbox id"
//	@Success	204	"deleted"
//	@Failure	404	{object}	ErrorResponse
//	@Failure	409	{object}	ErrorResponse	"projects live on this sandbox"
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/sandboxes/{id} [delete]
func (h *SandboxHandler) Delete(c *gin.Context) {
	projects, err := h.store.DeleteIfUnreferenced(c.Request.Context(), c.Param("id"))
	if err != nil {
		storeError(c, err)
		return
	}
	if projects > 0 {
		conflict(c, fmt.Sprintf("%d project(s) live on this sandbox; delete them first", projects))
		return
	}
	c.Status(http.StatusNoContent)
}

// Test runs a fixed health-check command to verify the sandbox works.
//
//	@Summary		Test sandbox
//	@Description	Runs "echo ok" in a throw-away sandbox — a container for a docker sandbox, a provisioned-and-destroyed instance for a remote service. 200 with ok=false means the service answered and the command did not.
//	@Tags			sandboxes
//	@Produce		json
//	@Param			id	path		string	true	"Sandbox id"
//	@Success		200	{object}	sandboxTestResp
//	@Failure		404	{object}	ErrorResponse
//	@Failure		502	{object}	ErrorResponse	"sandbox unreachable"
//	@Security		BearerAuth
//	@Router			/sandboxes/{id}/test [post]
func (h *SandboxHandler) Test(c *gin.Context) {
	ctx := c.Request.Context()
	sb, err := h.store.Get(ctx, c.Param("id"))
	if err != nil {
		storeError(c, err)
		return
	}
	// The check is the backend's: docker runs a throw-away container, a
	// remote service provisions and destroys a sandbox. 200 with ok=false means
	// the service answered and the command did not; an unreachable daemon or
	// bad credential is 502 — a different thing a caller must tell apart.
	if err := h.manager.Check(ctx, sb); err != nil {
		if errors.Is(err, sandboxes.ErrHealthCommandFailed) {
			c.JSON(http.StatusOK, sandboxTestResp{OK: false, Detail: err.Error()})
			return
		}
		upstreamError(c, err)
		return
	}
	c.JSON(http.StatusOK, sandboxTestResp{OK: true})
}

// Containers lists this package's containers on the sandbox's daemon —
// running and stopped, foreign containers never included.
//
//	@Summary	List the sandbox's managed containers
//	@Tags		sandboxes
//	@Produce	json
//	@Param		id	path		string	true	"Sandbox id"
//	@Success	200	{array}		dockersb.ManagedContainer
//	@Failure	404	{object}	ErrorResponse
//	@Failure	502	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/sandboxes/{id}/containers [get]
func (h *SandboxHandler) Containers(c *gin.Context) {
	opts, ok := h.daemon(c)
	if !ok {
		return
	}
	out, err := dockersb.ListManaged(c.Request.Context(), opts)
	if err != nil {
		upstreamError(c, err)
		return
	}
	if out == nil {
		out = []dockersb.ManagedContainer{}
	}
	c.JSON(http.StatusOK, out)
}

// daemon resolves the sandbox named by the id path parameter into its DOCKER
// connection options. The container listing and the stop/remove calls are a
// docker daemon's operator surface and exist nowhere else, so a sandbox of
// another type is refused by name rather than by a type error.
func (h *SandboxHandler) daemon(c *gin.Context) (dockersb.Options, bool) {
	sb, err := h.store.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		storeError(c, err)
		return dockersb.Options{}, false
	}
	opts, err := sandboxes.DaemonOptions(sb)
	if err != nil {
		badRequest(c, err.Error())
		return dockersb.Options{}, false
	}
	return opts, true
}

// containerAct resolves the daemon and the (prefix-checked) container name for
// the stop/remove endpoints.
func (h *SandboxHandler) containerAct(c *gin.Context) (dockersb.Options, string, bool) {
	opts, ok := h.daemon(c)
	if !ok {
		return dockersb.Options{}, "", false
	}
	name := c.Param("name")
	// The SDK re-verifies ownership by label; the prefix check just refuses
	// obviously-foreign names before a daemon round-trip.
	if !strings.HasPrefix(name, dockersb.ManagedNamePrefix) {
		badRequest(c, "not a managed container name")
		return dockersb.Options{}, "", false
	}
	return opts, name, true
}

// StopContainer stops one managed container; the next run starts it again.
//
//	@Summary	Stop a managed container
//	@Tags		sandboxes
//	@Param		id		path	string	true	"Sandbox id"
//	@Param		name	path	string	true	"Container name (agents-…)"
//	@Success	204		"stopped"
//	@Failure	400		{object}	ErrorResponse
//	@Failure	502		{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/sandboxes/{id}/containers/{name}/stop [post]
func (h *SandboxHandler) StopContainer(c *gin.Context) {
	opts, name, ok := h.containerAct(c)
	if !ok {
		return
	}
	if err := dockersb.StopManaged(c.Request.Context(), opts, name); err != nil {
		upstreamError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// RemoveContainer force-removes one managed container — the rebuild act: the
// project's volume survives, and the next run recreates the container from the
// current configuration.
//
//	@Summary	Remove a managed container (rebuild)
//	@Tags		sandboxes
//	@Param		id		path	string	true	"Sandbox id"
//	@Param		name	path	string	true	"Container name (agents-…)"
//	@Success	204		"removed"
//	@Failure	400		{object}	ErrorResponse
//	@Failure	502		{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/sandboxes/{id}/containers/{name} [delete]
func (h *SandboxHandler) RemoveContainer(c *gin.Context) {
	opts, name, ok := h.containerAct(c)
	if !ok {
		return
	}
	if err := dockersb.RemoveManaged(c.Request.Context(), opts, name); err != nil {
		upstreamError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// sandboxTestResp is the Test response: whether the health-check command
// succeeded, with failure detail when it didn't.
type sandboxTestResp struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}
