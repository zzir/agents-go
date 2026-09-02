package handler

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/skills"
)

// maxSkillBytes caps one stored SKILL.md, matching what a model read should
// ever inject into context.
const maxSkillBytes = 256 << 10

// SkillHandler manages stored SKILL.md documents: CRUD plus import from a
// GitHub repository or a raw URL (decisions §5.26).
type SkillHandler struct {
	store    *store.SkillStore
	settings *settings.Reader
	// githubAPI / githubRaw are the GitHub endpoints; tests point them at a
	// local fake.
	githubAPI string
	githubRaw string
}

// NewSkillHandler returns a handler over the skills store; settings supplies
// the outbound proxy for imports.
func NewSkillHandler(st *store.SkillStore, se *settings.Reader) *SkillHandler {
	return &SkillHandler{
		store: st, settings: se,
		githubAPI: "https://api.github.com",
		githubRaw: "https://raw.githubusercontent.com",
	}
}

// skillReq is the Create/Update request body: the document is the input, its
// frontmatter is the metadata.
type skillReq struct {
	Content string `json:"content" binding:"required"`
	// Scope on create only: "global" (admin) or the "private" default.
	Scope string `json:"scope,omitempty"`
}

// List responds with every stored skill's metadata (no content).
//
//	@Summary	List skills
//	@Tags		skills
//	@Produce	json
//	@Success	200	{array}	store.Skill
//	@Security	BearerAuth
//	@Router		/skills [get]
func (h *SkillHandler) List(c *gin.Context) {
	ownerID, admin, ok := callerScope(c)
	if !ok {
		return
	}
	out, err := h.store.ListMeta(c.Request.Context(), ownerID, admin)
	if err != nil {
		storeError(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// Get responds with one skill, content included.
//
//	@Summary	Get skill
//	@Tags		skills
//	@Produce	json
//	@Param		id	path		string	true	"Skill id"
//	@Success	200	{object}	store.Skill
//	@Failure	404	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/skills/{id} [get]
func (h *SkillHandler) Get(c *gin.Context) {
	if sk, ok := gatedRow(c, h.store.CrudStore, skillScope, visibleRow); ok {
		c.JSON(http.StatusOK, sk)
	}
}

// skillScope reads a skill's (scope, owner) pair for the scoped-CRUD gates.
func skillScope(s *store.Skill) (string, string) { return s.Scope, s.OwnerID }

// parseSkillContent validates a document for storage: size cap plus the
// SKILL.md frontmatter rules. Reports to c and returns ok=false on failure.
func parseSkillContent(c *gin.Context, content string) (skills.Skill, bool) {
	if len(content) > maxSkillBytes {
		badRequest(c, fmt.Sprintf("content exceeds %d KiB", maxSkillBytes>>10))
		return skills.Skill{}, false
	}
	meta, err := skills.Parse([]byte(content))
	if err != nil {
		badRequest(c, "invalid SKILL.md: "+err.Error())
		return skills.Skill{}, false
	}
	return meta, true
}

// Create stores a new skill authored in the workbench.
//
//	@Summary		Create skill
//	@Description	content is a full SKILL.md document; name and description are read from its frontmatter.
//	@Tags			skills
//	@Accept			json
//	@Produce		json
//	@Param			skill	body		skillReq	true	"SKILL.md content"
//	@Success		201		{object}	store.Skill
//	@Failure		400		{object}	ErrorResponse
//	@Failure		409		{object}	ErrorResponse	"name already in use"
//	@Security		BearerAuth
//	@Router			/skills [post]
func (h *SkillHandler) Create(c *gin.Context) {
	var req skillReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, "content is required")
		return
	}
	meta, ok := parseSkillContent(c, req.Content)
	if !ok {
		return
	}
	sk := &store.Skill{Name: meta.Name, Description: meta.Description, Content: req.Content, Scope: req.Scope}
	if !stampCreateScope(c, &sk.Scope, &sk.OwnerID) {
		return
	}
	if err := h.store.Create(c.Request.Context(), sk); err != nil {
		saveError(c, err)
		return
	}
	created(c, sk.ID, sk)
}

// Update overwrites a skill's content; name and description follow the new
// frontmatter. Editing an imported skill detaches it from its source, so a
// later re-import cannot overwrite the local edit.
//
//	@Summary	Update skill
//	@Tags		skills
//	@Accept		json
//	@Produce	json
//	@Param		id		path		string		true	"Skill id"
//	@Param		skill	body		skillReq	true	"SKILL.md content"
//	@Success	200		{object}	store.Skill
//	@Failure	400		{object}	ErrorResponse
//	@Failure	404		{object}	ErrorResponse
//	@Failure	409		{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/skills/{id} [put]
func (h *SkillHandler) Update(c *gin.Context) {
	var req skillReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, "content is required")
		return
	}
	meta, ok := parseSkillContent(c, req.Content)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	prev, ok := gatedRow(c, h.store.CrudStore, skillScope, editableRow)
	if !ok {
		return
	}
	sk := &store.Skill{Name: meta.Name, Description: meta.Description, Content: req.Content}
	// Scope, owner and source lineage come from the row INSIDE the store's
	// transaction, so a concurrent scope flip is not silently reverted.
	err := h.store.Update(ctx, prev.ID, sk, ownershipGuard(prev.Scope, prev.OwnerID, skillScope,
		func(p *store.Skill) error {
			sk.Scope, sk.OwnerID = p.Scope, p.OwnerID
			sk.SourceRepo, sk.SourcePath, sk.SourceSHA = p.SourceRepo, p.SourcePath, p.SourceSHA
			sk.Detached = p.Detached || (p.SourceRepo != "" && req.Content != p.Content)
			return nil
		}))
	if err != nil {
		saveError(c, err)
		return
	}
	sk.ID, sk.CreatedAt = prev.ID, prev.CreatedAt
	c.JSON(http.StatusOK, sk)
}

// Delete removes a skill. An agent whose selection still names the id simply
// stops advertising it — same as the skill never having been installed.
//
//	@Summary	Delete skill
//	@Tags		skills
//	@Param		id	path	string	true	"Skill id"
//	@Success	204	"deleted"
//	@Failure	404	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/skills/{id} [delete]
func (h *SkillHandler) Delete(c *gin.Context) {
	if deleteOwned(c, h.store.CrudStore, skillScope) {
		c.Status(http.StatusNoContent)
	}
}

// SetScope promotes a workbench-authored skill to global or demotes it back
// to its author's private set. An imported skill changes scope with its repo
// group (SetRepoScope) — never alone, so a group stays one scope. Agents
// still selecting a demoted skill stop advertising it — same as a delete.
//
//	@Summary	Change a skill's scope
//	@Tags		skills
//	@Accept		json
//	@Param		id		path	string		true	"Skill id"
//	@Param		scope	body	scopeReq	true	"global or private"
//	@Success	204
//	@Failure	400	{object}	ErrorResponse	"an imported skill flips with its repo"
//	@Failure	409	{object}	ErrorResponse	"name collision in the target scope"
//	@Security	BearerAuth
//	@Router		/skills/{id}/scope [post]
func (h *SkillHandler) SetScope(c *gin.Context) {
	// Visibility decides FIRST: a foreign private row reads as absent, so the
	// imported/authored refusal below is never an existence oracle.
	sk, ok := gatedRow(c, h.store.CrudStore, skillScope, visibleRow)
	if !ok {
		return
	}
	if sk.SourceRepo != "" {
		badRequest(c, "an imported skill changes scope with its repository — use the repository scope action")
		return
	}
	setScopePlain(c, h.store.CrudStore, "skill", skillScope)
}

// repoScopeReq is the body of POST /skill-repos/scope: the repo group to
// flip, and where to. OwnerID names whose group a promote publishes — for an
// admin promoting a member's import; empty means the caller's own.
type repoScopeReq struct {
	Repo    string `json:"repo" binding:"required"`
	Scope   string `json:"scope" binding:"required"`
	OwnerID string `json:"owner_id,omitempty"`
}

// SetRepoScope flips a whole repo group between private and global — all or
// nothing, so a group is always one scope (decisions §5.29). Promote is
// admin-only; demote is the admin's or the group owner's, returning the rows
// to their author.
//
//	@Summary	Change an imported repo's scope (all of its skills at once)
//	@Tags		skills
//	@Accept		json
//	@Param		body	body	repoScopeReq	true	"Repo, target scope, and (promote, admin) whose group"
//	@Success	204
//	@Failure	400	{object}	ErrorResponse
//	@Failure	404	{object}	ErrorResponse	"no such repo group"
//	@Failure	409	{object}	ErrorResponse	"already in that scope, or a name collision in the target scope"
//	@Security	BearerAuth
//	@Router		/skill-repos/scope [post]
func (h *SkillHandler) SetRepoScope(c *gin.Context) {
	var req repoScopeReq
	if err := c.ShouldBindJSON(&req); err != nil || (req.Scope != store.ScopeGlobal && req.Scope != store.ScopePrivate) {
		badRequest(c, `repo is required and scope must be "global" or "private"`)
		return
	}
	ctx := c.Request.Context()
	ownerID, _, ok := callerScope(c)
	if !ok {
		return
	}
	// The group is named, never guessed: (repo, owner), defaulting to the
	// caller's own. A group nobody named is a group nobody flips — which is
	// what keeps an admin who holds their own copy of a repo from moving
	// somebody else's by accident (decisions §5.31).
	groupOwner := req.OwnerID
	if groupOwner == "" {
		groupOwner = ownerID
	}
	cur, found, err := h.store.RepoGroup(ctx, req.Repo, groupOwner)
	if err != nil {
		internalError(c, err)
		return
	}
	if !found {
		notFound(c)
		return
	}
	if !scopeChangeAllowed(c, req.Scope, cur, groupOwner) {
		return
	}
	n, err := h.store.SetRepoScope(ctx, req.Repo, groupOwner, req.Scope)
	if err != nil {
		saveError(c, err) // name collision in the target scope -> 409
		return
	}
	if n == 0 {
		conflict(c, "the repository's skills are already "+req.Scope)
		return
	}
	c.Status(http.StatusNoContent)
}

// SetOwner transfers a skill to another account (admin). An imported skill
// moves with its whole repo group — the group is the unit of ownership as it
// is of scope, so one row may not leave it behind (decisions §5.31).
//
//	@Summary	Reassign a skill's owner (admin); an imported skill moves with its repo
//	@Tags		skills
//	@Accept		json
//	@Param		id		path	string			true	"Skill id"
//	@Param		body	body	SetOwnerRequest	true	"The new owner"
//	@Success	204
//	@Failure	400	{object}	ErrorResponse	"malformed body, or no such user"
//	@Failure	409	{object}	ErrorResponse	"name collision in the target owner's namespace"
//	@Security	BearerAuth
//	@Router		/skills/{id}/owner [put]
func (h *SkillHandler) SetOwner(c *gin.Context) {
	ctx := c.Request.Context()
	sk, err := h.store.Get(ctx, c.Param("id"))
	if err != nil {
		storeError(c, err)
		return
	}
	if sk.SourceRepo == "" {
		setOwnerPlain(c, h.store.CrudStore)
		return
	}
	var req SetOwnerRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.UserID == "" {
		badRequest(c, "user_id is required")
		return
	}
	if err := h.store.SetRepoOwner(ctx, sk.SourceRepo, sk.OwnerID, req.UserID); err != nil {
		if errors.Is(err, store.ErrNoSuchUser) {
			badRequest(c, "no such user")
			return
		}
		saveError(c, err) // name collision in the target owner's namespace -> 409
		return
	}
	server.SetAuditDetail(c, "owner="+req.UserID)
	c.Status(http.StatusNoContent)
}
