package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/sandboxes"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	dockersb "github.com/zzir/agents-go/sandbox/docker"
)

// SandboxTargetHandler serves CRUD endpoints and the health check for sandbox
// targets — the machines projects run on (decisions §5.33).
type SandboxTargetHandler struct {
	store     *store.SandboxTargetStore
	templates *store.SandboxTemplateStore
	manager   *sandboxes.Manager
	retire    *Retirer
}

// NewSandboxTargetHandler returns a handler over the target store; templates
// supply the image the health check needs, and the retirer is what carries a
// content change to the projects on this target.
func NewSandboxTargetHandler(s *store.SandboxTargetStore, templates *store.SandboxTemplateStore, m *sandboxes.Manager, r *Retirer) *SandboxTargetHandler {
	if s == nil || templates == nil || m == nil || r == nil {
		panic("handler: NewSandboxTargetHandler needs the target and template stores, the manager and a retirer")
	}
	return &SandboxTargetHandler{store: s, templates: templates, manager: m, retire: r}
}

// typeMismatch names a target and template that cannot be paired. The check
// happens on a project write and on the health check; one wording keeps the
// two from drifting.
func typeMismatch(target *store.SandboxTarget, tpl *store.SandboxTemplate) string {
	return fmt.Sprintf("the %s template %q cannot run on the %s target %q", tpl.Type, tpl.Name, target.Type, target.Name)
}

// Retirer turns a content change on a target or template into the project
// generations it invalidates, then retires each project's live instance and
// terminals. It is the one place the "one runtime axis" rule is applied
// (decisions §5.33).
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

// bump moves the runtime generation of every project whose column holds id and
// retires what each was serving. A failure is returned to the caller, which
// answers 500: the row is already written, and silently leaving live
// containers on the old configuration is the worse outcome to hide.
func (r *Retirer) bump(ctx context.Context, column, id string) error {
	gens, err := r.projects.BumpRuntimeGen(ctx, column, id)
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

// List responds with all sandbox targets, secrets masked.
//
//	@Summary	List sandbox targets
//	@Tags		sandbox-targets
//	@Produce	json
//	@Success	200	{array}		store.SandboxTarget
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/sandbox-targets [get]
func (h *SandboxTargetHandler) List(c *gin.Context) {
	targets, err := h.store.List(c.Request.Context())
	if err != nil {
		internalError(c, err)
		return
	}
	for i := range targets {
		targets[i] = sanitizeTargetConfig(targets[i])
	}
	c.JSON(http.StatusOK, targets)
}

type sandboxTargetReq struct {
	Name   string          `json:"name"`
	Type   string          `json:"type"`
	Config json.RawMessage `json:"config"`
	// Revision, on update only, is the revision the client's edit was based
	// on (from GET/List): editing from a stale form is then 409 instead of
	// silently overwriting a concurrent update. Absent (0) keeps
	// last-writer-wins. Ignored on create.
	Revision int64 `json:"revision,omitempty"`
}

func (r sandboxTargetReq) toTarget() *store.SandboxTarget {
	return &store.SandboxTarget{Name: r.Name, Type: r.Type, Config: r.Config}
}

// validate enforces the POLICY layer of a target write: a name, and a type
// this build carries. Field-level validation and canonicalization live in
// store.NormalizeTargetConfig, which both write handlers run right after this.
func (h *SandboxTargetHandler) validate(c *gin.Context, req *sandboxTargetReq) bool {
	if req.Name == "" {
		badRequest(c, "name is required")
		return false
	}
	if req.Type == "" {
		badRequest(c, "type is required")
		return false
	}
	if !slices.Contains(store.TargetTypes, req.Type) {
		badRequest(c, "type must be one of "+strings.Join(store.TargetTypes, ", ")+", got "+req.Type)
		return false
	}
	return true
}

// Create persists a new sandbox target from the request body.
//
//	@Summary		Create sandbox target
//	@Description	type is "docker" (config: host — "" = local daemon, tcp://, or ssh://user@host with ssh_* auth) or "e2b" (config: api_url, domain, api_key, data_plane_auth). ssh_password and api_key are write-only, ******** mask semantics.
//	@Tags			sandbox-targets
//	@Accept			json
//	@Produce		json
//	@Param			target	body		sandboxTargetReq	true	"Sandbox target"
//	@Success		201		{object}	store.SandboxTarget
//	@Failure		400		{object}	ErrorResponse
//	@Failure		500		{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/sandbox-targets [post]
func (h *SandboxTargetHandler) Create(c *gin.Context) {
	var req sandboxTargetReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err.Error())
		return
	}
	if !h.validate(c, &req) {
		return
	}
	t := req.toTarget()
	// No stored config yet: mask sentinels resolve to empty.
	t.Config = restoreTargetConfig(t.Config, nil)
	canonical, err := store.NormalizeTargetConfig(t.Type, t.Config)
	if err != nil {
		badRequest(c, err.Error())
		return
	}
	t.Config = canonical
	if err := h.store.Create(c.Request.Context(), t); err != nil {
		internalError(c, err)
		return
	}
	view := sanitizeTargetConfig(*t)
	created(c, view.ID, view)
}

// Get responds with one sandbox target, secrets masked.
//
//	@Summary	Get sandbox target
//	@Tags		sandbox-targets
//	@Produce	json
//	@Param		id	path		string	true	"Target id"
//	@Success	200	{object}	store.SandboxTarget
//	@Failure	404	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/sandbox-targets/{id} [get]
func (h *SandboxTargetHandler) Get(c *gin.Context) {
	t, err := h.store.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		storeError(c, err)
		return
	}
	c.JSON(http.StatusOK, sanitizeTargetConfig(*t))
}

// Update overwrites the target and responds with the updated row. A masked
// SSH password keeps the stored value. An update that would change the
// target's identity (type, daemon host — see TargetIdentityChanged) is
// refused with 409 while any project lives on it: a project's files are on
// that machine, and rewriting what the id points at would move every one of
// them. Non-identity fields update freely.
//
//	@Summary		Update sandbox target
//	@Description	Include the revision the edit was based on (from GET/List) to make the write conditional: 409 if the row changed meanwhile. Omitting it falls back to last-writer-wins.
//	@Tags			sandbox-targets
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string				true	"Target id"
//	@Param			target	body		sandboxTargetReq	true	"Sandbox target"
//	@Success		200		{object}	store.SandboxTarget
//	@Failure		400		{object}	ErrorResponse
//	@Failure		404		{object}	ErrorResponse
//	@Failure		409		{object}	ErrorResponse	"identity change refused (projects live on it), or the row changed concurrently — re-read and retry"
//	@Failure		500		{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/sandbox-targets/{id} [put]
func (h *SandboxTargetHandler) Update(c *gin.Context) {
	var req sandboxTargetReq
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
	// would resolve the ******** mask to "" and silently WIPE the stored ssh
	// password — the same guard every sibling handler carries.
	prev, err := h.store.Get(ctx, id)
	if err != nil {
		storeError(c, err)
		return
	}
	t := req.toTarget()
	// A masked credential means "keep the stored one", which only holds while
	// the destination is unchanged: moving the machine or the service must not
	// carry the old key along. Only refuse when one is actually stored — a
	// mask with nothing behind it resolves to "" and needs no guard.
	if maskAcrossDestination(t.Config, prev.Config, store.TargetDestinationField(prev.Type)) && storedTargetSecret(prev.Config) {
		badRequest(c, "the destination changed: the stored credential belongs to the previous one — replace it or clear it")
		return
	}
	t.Config = restoreTargetConfig(t.Config, prev.Config)
	// Normalize AFTER the mask restore: the canonical form must carry the
	// real secret, not the ******** sentinel.
	canonical, err := store.NormalizeTargetConfig(t.Type, t.Config)
	if err != nil {
		badRequest(c, err.Error())
		return
	}
	t.Config = canonical
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
	contentChanged := prev.Type != t.Type || !store.TargetContentEqual(t.Type, prev.Config, t.Config)
	if store.TargetIdentityChanged(prev, t) {
		projects, uerr := h.store.UpdateIdentityIfUnreferenced(ctx, id, t, expected)
		if uerr != nil {
			storeError(c, uerr)
			return
		}
		if projects > 0 {
			conflict(c, fmt.Sprintf("%d project(s) live on this target; its type and machine are frozen — credentials and name stay editable, or create a new target for the new location", projects))
			return
		}
	} else if err := h.store.Update(ctx, id, t, expected); err != nil {
		storeError(c, err)
		return
	}
	// The write landed; invalidate NOW, from what the CAS guarantees — not
	// from a re-read that a cancelled request could fail, leaving new
	// credentials in the store while old instances and terminals keep
	// serving. Only a CONTENT change retires: a rename must not sever
	// terminals or close idle containers.
	if contentChanged {
		if err := h.retire.bump(ctx, "target_id", id); err != nil {
			internalError(c, err)
			return
		}
	}
	updated, err := h.store.Get(ctx, id)
	if err != nil {
		storeError(c, err)
		return
	}
	c.JSON(http.StatusOK, sanitizeTargetConfig(*updated))
}

// Delete removes the sandbox target. One still carrying projects is refused
// with 409: their working trees live on that machine, and the operator
// deletes them (which reclaims their storage) first. The refusal is decided
// by the delete statement itself, not a prior count — a project create racing
// this delete therefore either lands before it (and blocks it) or loses its
// own lock.
//
//	@Summary	Delete sandbox target
//	@Tags		sandbox-targets
//	@Param		id	path	string	true	"Target id"
//	@Success	204	"deleted"
//	@Failure	404	{object}	ErrorResponse
//	@Failure	409	{object}	ErrorResponse	"projects live on this target"
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/sandbox-targets/{id} [delete]
func (h *SandboxTargetHandler) Delete(c *gin.Context) {
	projects, err := h.store.DeleteIfUnreferenced(c.Request.Context(), c.Param("id"))
	if err != nil {
		storeError(c, err)
		return
	}
	if projects > 0 {
		conflict(c, fmt.Sprintf("%d project(s) live on this target; delete them first", projects))
		return
	}
	c.Status(http.StatusNoContent)
}

// Test runs a fixed health-check command on the target to verify
// connectivity. The image comes from a template, since a target names none.
//
//	@Summary		Test sandbox target
//	@Description	Runs "echo ok" in a throw-away sandbox from the named template — a container for a docker target, a provisioned-and-destroyed sandbox for a remote service. 200 with ok=false means the service answered and the command did not. The template's type must match the target's.
//	@Tags			sandbox-targets
//	@Produce		json
//	@Param			id			path		string	true	"Target id"
//	@Param			template_id	query		string	true	"Template to take the image from"
//	@Success		200			{object}	sandboxTestResp
//	@Failure		400			{object}	ErrorResponse
//	@Failure		404			{object}	ErrorResponse
//	@Failure		502			{object}	ErrorResponse	"target unreachable"
//	@Security		BearerAuth
//	@Router			/sandbox-targets/{id}/test [post]
func (h *SandboxTargetHandler) Test(c *gin.Context) {
	ctx := c.Request.Context()
	target, err := h.store.Get(ctx, c.Param("id"))
	if err != nil {
		storeError(c, err)
		return
	}
	templateID := c.Query("template_id")
	if templateID == "" {
		badRequest(c, "template_id is required: a target names no image")
		return
	}
	tpl, err := h.templates.Get(ctx, templateID)
	if err != nil {
		storeError(c, err)
		return
	}
	if tpl.Type != target.Type {
		badRequest(c, typeMismatch(target, tpl))
		return
	}
	// The check is the backend's: a docker target runs a throw-away
	// container, a remote service provisions and destroys a sandbox. 200 with
	// ok=false means the service answered and the command did not.
	if err := h.manager.CheckTarget(ctx, target, tpl); err != nil {
		c.JSON(http.StatusOK, sandboxTestResp{OK: false, Detail: err.Error()})
		return
	}
	c.JSON(http.StatusOK, sandboxTestResp{OK: true})
}

// Containers lists this package's containers on the target's daemon —
// running and stopped, foreign containers never included.
//
//	@Summary	List the target's managed containers
//	@Tags		sandbox-targets
//	@Produce	json
//	@Param		id	path		string	true	"Target id"
//	@Success	200	{array}		dockersb.ManagedContainer
//	@Failure	404	{object}	ErrorResponse
//	@Failure	502	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/sandbox-targets/{id}/containers [get]
func (h *SandboxTargetHandler) Containers(c *gin.Context) {
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

// daemon resolves the target named by the id path parameter into its DOCKER
// connection options. The container listing and the stop/remove calls are a
// docker daemon's operator surface and exist nowhere else, so a target of
// another type is refused by name rather than by a type error.
func (h *SandboxTargetHandler) daemon(c *gin.Context) (dockersb.Options, bool) {
	t, err := h.store.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		storeError(c, err)
		return dockersb.Options{}, false
	}
	opts, err := sandboxes.TargetOptions(t)
	if err != nil {
		badRequest(c, err.Error())
		return dockersb.Options{}, false
	}
	return opts, true
}

// containerAct resolves the daemon and the (prefix-checked) container name for
// the stop/remove endpoints.
func (h *SandboxTargetHandler) containerAct(c *gin.Context) (dockersb.Options, string, bool) {
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
//	@Tags		sandbox-targets
//	@Param		id		path	string	true	"Target id"
//	@Param		name	path	string	true	"Container name (agents-…)"
//	@Success	204		"stopped"
//	@Failure	400		{object}	ErrorResponse
//	@Failure	502		{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/sandbox-targets/{id}/containers/{name}/stop [post]
func (h *SandboxTargetHandler) StopContainer(c *gin.Context) {
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
//	@Tags		sandbox-targets
//	@Param		id		path	string	true	"Target id"
//	@Param		name	path	string	true	"Container name (agents-…)"
//	@Success	204		"removed"
//	@Failure	400		{object}	ErrorResponse
//	@Failure	502		{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/sandbox-targets/{id}/containers/{name} [delete]
func (h *SandboxTargetHandler) RemoveContainer(c *gin.Context) {
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
