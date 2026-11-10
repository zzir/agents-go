package handler

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/skills"
)

// maxSkillBytes caps one stored SKILL.md, matching what a model read should
// ever inject into context.
const maxSkillBytes = 256 << 10

// SkillHandler manages stored SKILL.md documents: CRUD plus import from a
// GitHub repository or a raw URL (decisions §5.26).
type SkillHandler struct {
	store *store.SkillStore
	// githubAPI / githubRaw are the GitHub endpoints; tests point them at a
	// local fake.
	githubAPI string
	githubRaw string
}

// NewSkillHandler returns a handler over the skills store.
func NewSkillHandler(st *store.SkillStore) *SkillHandler {
	return &SkillHandler{
		store:     st,
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
func (h *SkillHandler) List(c *gin.Context) error {
	ownerID, admin, err := callerScope(c)
	if err != nil {
		return err
	}
	out, err := h.store.ListMeta(c.Request.Context(), ownerID, admin)
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, nonNilList(out))
	return nil
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
func (h *SkillHandler) Get(c *gin.Context) error {
	sk, err := gatedRow(c, h.store.CrudStore, skillScope, visibleRow)
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, sk)
	return nil
}

// skillScope reads a skill's (scope, owner) pair for the scoped-CRUD gates.
func skillScope(s *store.Skill) (string, string) { return s.Scope, s.OwnerID }

// parseSkillContent validates a document for storage: size cap plus the
// SKILL.md frontmatter rules.
func parseSkillContent(content string) (skills.Skill, error) {
	if len(content) > maxSkillBytes {
		return skills.Skill{}, badRequest(fmt.Sprintf("content exceeds %d KiB", maxSkillBytes>>10))
	}
	meta, err := skills.Parse([]byte(content))
	if err != nil {
		return skills.Skill{}, badRequest("invalid SKILL.md: " + err.Error())
	}
	return meta, nil
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
func (h *SkillHandler) Create(c *gin.Context) error {
	var req skillReq
	if err := c.ShouldBindJSON(&req); err != nil {
		return badRequest("content is required")
	}
	meta, err := parseSkillContent(req.Content)
	if err != nil {
		return err
	}
	sk := &store.Skill{Name: meta.Name, Description: meta.Description, Content: req.Content, Scope: req.Scope}
	if err := stampCreateScope(c, &sk.Scope, &sk.OwnerID); err != nil {
		return err
	}
	if err := h.store.Create(c.Request.Context(), sk); err != nil {
		return err
	}
	created(c, sk.ID, sk)
	return nil
}

// Update overwrites a skill's content; name and description follow the new
// frontmatter. Editing an imported skill detaches it from its source.
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
func (h *SkillHandler) Update(c *gin.Context) error {
	var req skillReq
	if err := c.ShouldBindJSON(&req); err != nil {
		return badRequest("content is required")
	}
	meta, err := parseSkillContent(req.Content)
	if err != nil {
		return err
	}
	ctx := c.Request.Context()
	prev, err := gatedRow(c, h.store.CrudStore, skillScope, editableRow)
	if err != nil {
		return err
	}
	sk := &store.Skill{Name: meta.Name, Description: meta.Description, Content: req.Content}
	// Scope, owner and source lineage come from the row inside the transaction.
	err = h.store.Update(ctx, prev.ID, sk, ownershipGuard(prev.Scope, prev.OwnerID, skillScope,
		func(p *store.Skill) error {
			sk.Scope, sk.OwnerID = p.Scope, p.OwnerID
			sk.SourceRepo, sk.SourcePath, sk.SourceSHA = p.SourceRepo, p.SourcePath, p.SourceSHA
			sk.Detached = p.Detached || (p.SourceRepo != "" && req.Content != p.Content)
			return nil
		}))
	if err != nil {
		return err
	}
	sk.ID, sk.CreatedAt = prev.ID, prev.CreatedAt
	c.JSON(http.StatusOK, sk)
	return nil
}

// Delete removes a skill; an agent whose selection still names it stops
// advertising it (invariant 13).
//
//	@Summary	Delete skill
//	@Tags		skills
//	@Param		id	path	string	true	"Skill id"
//	@Success	204	"deleted"
//	@Failure	404	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/skills/{id} [delete]
func (h *SkillHandler) Delete(c *gin.Context) error {
	if err := deleteOwned(c, h.store.CrudStore, skillScope, h.store.DeleteOwnedBy); err != nil {
		return err
	}
	c.Status(http.StatusNoContent)
	return nil
}

// SetScope flips a workbench-authored skill between private and global; an
// imported one moves with its repo group (SetRepoScope) — decisions §5.31.
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
func (h *SkillHandler) SetScope(c *gin.Context) error {
	// Visibility decides first, so the refusal below is never an existence
	// oracle (decisions §5.29).
	sk, err := gatedRow(c, h.store.CrudStore, skillScope, visibleRow)
	if err != nil {
		return err
	}
	if sk.SourceRepo != "" {
		return badRequest("an imported skill changes scope with its repository — use the repository scope action")
	}
	return setScopePlain(c, h.store.CrudStore, "skill", skillScope, h.store.SetScope)
}

// repoScopeReq is the body of POST /skill-repos/scope: the repo group to
// flip, and where to. OwnerID names whose group; empty means the caller's own.
type repoScopeReq struct {
	Repo    string `json:"repo" binding:"required"`
	Scope   string `json:"scope" binding:"required"`
	OwnerID string `json:"owner_id,omitempty"`
}

// SetRepoScope flips a whole repo group between private and global, all or
// nothing — decisions §5.31.
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
func (h *SkillHandler) SetRepoScope(c *gin.Context) error {
	var req repoScopeReq
	if err := c.ShouldBindJSON(&req); err != nil || (req.Scope != store.ScopeGlobal && req.Scope != store.ScopePrivate) {
		return badRequest(`repo is required and scope must be "global" or "private"`)
	}
	ctx := c.Request.Context()
	ownerID, _, err := callerScope(c)
	if err != nil {
		return err
	}
	// The group is (repo, owner), defaulting to the caller's own — decisions §5.31.
	groupOwner := req.OwnerID
	if groupOwner == "" {
		groupOwner = ownerID
	}
	cur, found, err := h.store.RepoGroup(ctx, req.Repo, groupOwner)
	if err != nil {
		return err
	}
	if !found {
		return errNotFound
	}
	if err := scopeChangeAllowed(c, req.Scope, cur, groupOwner); err != nil {
		return err
	}
	n, err := h.store.SetRepoScope(ctx, req.Repo, groupOwner, req.Scope)
	if err != nil {
		return err // name collision in the target scope -> 409
	}
	if n == 0 {
		return conflict("the repository's skills are already " + req.Scope)
	}
	server.SetAuditResource(c, req.Repo)
	server.SetAuditDetail(c, "owner="+groupOwner+" scope="+req.Scope)
	c.Status(http.StatusNoContent)
	return nil
}

// SetOwner transfers a skill to another account (admin) — decisions §5.31.
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
func (h *SkillHandler) SetOwner(c *gin.Context) error {
	ctx := c.Request.Context()
	sk, err := h.store.Get(ctx, c.Param("id"))
	if err != nil {
		return err
	}
	if sk.SourceRepo == "" {
		return setOwnerPlain(c, h.store.SetOwner)
	}
	var req SetOwnerRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.UserID == "" {
		return badRequest("user_id is required")
	}
	if err := h.store.SetRepoOwner(ctx, sk.SourceRepo, sk.OwnerID, req.UserID); err != nil {
		if errors.Is(err, store.ErrNoSuchUser) {
			return badRequest("no such user")
		}
		return err // name collision in the target owner's namespace -> 409
	}
	server.SetAuditDetail(c, "owner="+req.UserID)
	c.Status(http.StatusNoContent)
	return nil
}
