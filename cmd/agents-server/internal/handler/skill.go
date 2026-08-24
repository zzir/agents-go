package handler

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/skills"
)

// maxSkillBytes caps one stored SKILL.md, matching what a model read should
// ever inject into context.
const maxSkillBytes = 256 << 10

// SkillHandler manages stored SKILL.md documents: CRUD plus import from a
// GitHub repository or a raw URL (spec §5.26).
type SkillHandler struct {
	store    *store.SkillStore
	settings *settings.Reader
	// githubAPI / githubRaw are the GitHub endpoints; tests point them at a
	// local fake.
	githubAPI string
	githubRaw string
}

// NewSkillHandler returns a handler over the skills store; settings supplies
// the optional GitHub token and the outbound proxy for imports.
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
	sk, err := h.store.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		storeError(c, err)
		return
	}
	if !visibleRow(c, sk.Scope, sk.OwnerID) {
		return
	}
	c.JSON(http.StatusOK, sk)
}

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
	prev, err := h.store.Get(ctx, c.Param("id"))
	if err != nil {
		storeError(c, err)
		return
	}
	if !editableRow(c, prev.Scope, prev.OwnerID) {
		return
	}
	sk := &store.Skill{Name: meta.Name, Description: meta.Description, Content: req.Content}
	// Scope, owner and source lineage come from the row INSIDE the store's
	// transaction, so a concurrent scope flip is not silently reverted.
	err = h.store.Update(ctx, prev.ID, sk, func(p *store.Skill) error {
		sk.Scope, sk.OwnerID = p.Scope, p.OwnerID
		sk.SourceRepo, sk.SourcePath, sk.SourceSHA = p.SourceRepo, p.SourcePath, p.SourceSHA
		sk.Detached = p.Detached || (p.SourceRepo != "" && req.Content != p.Content)
		return nil
	})
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
	cur, err := h.store.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		storeError(c, err)
		return
	}
	if !deletableRow(c, cur.Scope, cur.OwnerID) {
		return
	}
	if err := h.store.Delete(c.Request.Context(), c.Param("id")); err != nil {
		storeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// SetScope promotes a skill to global or demotes it to the acting admin's
// private set. Agents still selecting a demoted skill stop advertising it —
// same as a delete.
//
//	@Summary	Change a skill's scope
//	@Tags		skills
//	@Accept		json
//	@Param		id		path	string		true	"Skill id"
//	@Param		scope	body	scopeReq	true	"global or private"
//	@Success	204
//	@Failure	400	{object}	ErrorResponse
//	@Failure	409	{object}	ErrorResponse	"name collision in the target scope"
//	@Security	BearerAuth
//	@Router		/skills/{id}/scope [post]
func (h *SkillHandler) SetScope(c *gin.Context) {
	setScopePlain(c, h.store.CrudStore, "skill", func(sk *store.Skill) (string, string) { return sk.Scope, sk.OwnerID })
}
