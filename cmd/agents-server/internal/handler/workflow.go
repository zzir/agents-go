package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// WorkflowHandler serves the workflow DEFINITIONS and starts executions.
// Executions are tasks (kind "workflow"), managed through the task endpoints.
type WorkflowHandler struct {
	store    *store.WorkflowStore
	agents   *store.AgentConfigStore
	sessions *store.SessionStore
	runner   *bridge.Runner
}

// NewWorkflowHandler returns a handler backed by the given stores; the runner
// starts the executions.
func NewWorkflowHandler(s *store.WorkflowStore, agents *store.AgentConfigStore, sessions *store.SessionStore, runner *bridge.Runner) *WorkflowHandler {
	return &WorkflowHandler{store: s, agents: agents, sessions: sessions, runner: runner}
}

// runWorkflowReq is the body of a manual run: the session the result comes back
// to, and the brief — what this execution is about, written by the person.
type runWorkflowReq struct {
	SessionID string `json:"session_id" binding:"required"`
	Input     string `json:"input"`
	// WorkspaceID binds a still-unbound session first, so the execution has its
	// file and command tools; a bound session ignores it.
	WorkspaceID string `json:"workspace_id"`
}

// Run starts an execution of the workflow for a session, with the person's brief.
//
//	@Summary		Run a workflow (optionally binding the session's workspace first)
//	@Description	Starts an execution as a background task of the given session, with the brief in the body (the workflow's session cannot see the conversation, so the brief is what it works from). The result comes back to the session as a notification, like any task's. 400 when the workflow has no runnable steps or an agent is gone, 404 for an unknown workflow or session, 409 when the session is at its background-task cap or being deleted, 503 while the server shuts down.
//	@Tags			workflows
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string			true	"Workflow ID"
//	@Param			request	body		runWorkflowReq	true	"Session and brief"
//	@Success		201		{object}	bridge.TaskInfo
//	@Failure		400		{object}	ErrorResponse
//	@Failure		404		{object}	ErrorResponse
//	@Failure		409		{object}	ErrorResponse
//	@Failure		503		{object}	ErrorResponse	"the server is shutting down"
//	@Security		BearerAuth
//	@Router			/workflows/{id}/runs [post]
func (h *WorkflowHandler) Run(c *gin.Context) error {
	var req runWorkflowReq
	if err := c.ShouldBindJSON(&req); err != nil {
		return badRequest(err.Error())
	}
	// Into a session the caller owns; a foreign one reads as absent — and a
	// workflow the caller may not see reads the same way.
	if _, err := requireOwnedSession(c, h.sessions, req.SessionID); err != nil {
		return err
	}
	if _, err := gatedRow(c, h.store.CrudStore, workflowScope, visibleRow); err != nil {
		return err
	}
	// A still-unbound session takes the workspace the start carries; a bound
	// one keeps its binding.
	if req.WorkspaceID != "" {
		if _, err := h.runner.BindSessionWorkspace(c.Request.Context(), req.SessionID, req.WorkspaceID); err != nil {
			return err
		}
	}
	info, err := h.runner.RunWorkflow(c.Request.Context(), c.Param("id"), req.SessionID, strings.TrimSpace(req.Input), store.WorkflowOrigin{Kind: store.OriginPerson})
	if err != nil {
		if errors.Is(err, bridge.ErrWorkflowUnavailable) {
			return badRequest(err.Error())
		}
		return err
	}
	c.JSON(http.StatusCreated, info)
	return nil
}

// bind decodes and validates an incoming definition, every step's agent
// included, reporting the failure itself.
func (h *WorkflowHandler) bind(c *gin.Context, wf *store.Workflow) error {
	if err := c.ShouldBindJSON(wf); err != nil {
		return badRequest(err.Error())
	}
	if err := store.NormalizeWorkflow(wf); err != nil {
		return badRequest(err.Error())
	}
	return nil
}

// validateStepAgents checks each step's agent exists AND is one this
// workflow's scope may reference (decisions §5.29); runs after the scope is stamped.
func (h *WorkflowHandler) validateStepAgents(c *gin.Context, wf *store.Workflow) error {
	for _, step := range wf.Steps {
		absent := badRequest("step " + step.ID + ": agent_config_id names no agent")
		ac, err := h.agents.Get(c.Request.Context(), step.AgentConfigID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return absent
			}
			return err
		}
		switch reason, hidden := refRefusal(c, "agent", ac.Name, ac.Scope, ac.OwnerID, wf.Scope, wf.OwnerID); {
		case hidden:
			return absent
		case reason != "":
			return badRequest("step " + step.ID + ": " + reason)
		}
	}
	return nil
}

// List responds with every workflow definition.
//
//	@Summary	List workflows
//	@Tags		workflows
//	@Produce	json
//	@Success	200	{array}		store.Workflow
//	@Failure	500	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/workflows [get]
func (h *WorkflowHandler) List(c *gin.Context) error {
	list, err := listVisible(c, h.store.CrudStore)
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, list)
	return nil
}

// Get responds with one workflow definition.
//
//	@Summary	Get a workflow
//	@Tags		workflows
//	@Produce	json
//	@Param		id	path		string	true	"Workflow ID"
//	@Success	200	{object}	store.Workflow
//	@Failure	404	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/workflows/{id} [get]
func (h *WorkflowHandler) Get(c *gin.Context) error {
	wf, err := gatedRow(c, h.store.CrudStore, workflowScope, visibleRow)
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, wf)
	return nil
}

// workflowScope reads a workflow's (scope, owner) pair for the scoped-CRUD gates.
func workflowScope(w *store.Workflow) (string, string) { return w.Scope, w.OwnerID }

// Create stores a new workflow definition.
//
//	@Summary		Create a workflow
//	@Description	An ordered sequence of steps, each naming the agent that runs it and the prompt that starts its turn. A step without an id is given a stable one.
//	@Tags			workflows
//	@Accept			json
//	@Produce		json
//	@Param			workflow	body		store.Workflow	true	"Workflow"
//	@Success		201			{object}	store.Workflow
//	@Failure		400			{object}	ErrorResponse
//	@Failure		409			{object}	ErrorResponse	"duplicate name"
//	@Security		BearerAuth
//	@Router			/workflows [post]
func (h *WorkflowHandler) Create(c *gin.Context) error {
	var wf store.Workflow
	if err := h.bind(c, &wf); err != nil {
		return err
	}
	wf.ID, wf.CreatedAt = "", time.Time{} // server-owned
	if err := stampCreateScope(c, &wf.Scope, &wf.OwnerID); err != nil {
		return err
	}
	if err := h.validateStepAgents(c, &wf); err != nil {
		return err
	}
	if err := h.store.Create(c.Request.Context(), &wf); err != nil {
		return err // duplicate name -> 409
	}
	created(c, wf.ID, wf)
	return nil
}

// Update overwrites a workflow definition.
//
//	@Summary		Update a workflow
//	@Description	Executions already in flight are unaffected — each carries a snapshot of the definition it started with.
//	@Tags			workflows
//	@Accept			json
//	@Produce		json
//	@Param			id			path		string			true	"Workflow ID"
//	@Param			workflow	body		store.Workflow	true	"Workflow"
//	@Success		200			{object}	store.Workflow
//	@Failure		400			{object}	ErrorResponse
//	@Failure		404			{object}	ErrorResponse
//	@Failure		409			{object}	ErrorResponse	"duplicate name"
//	@Security		BearerAuth
//	@Router			/workflows/{id} [put]
func (h *WorkflowHandler) Update(c *gin.Context) error {
	var wf store.Workflow
	if err := h.bind(c, &wf); err != nil {
		return err
	}
	ctx, id := c.Request.Context(), c.Param("id")
	cur, err := gatedRow(c, h.store.CrudStore, workflowScope, editableRow)
	if err != nil {
		return err
	}
	wf.Scope, wf.OwnerID = cur.Scope, cur.OwnerID
	if err := h.validateStepAgents(c, &wf); err != nil {
		return err
	}
	// Scope and owner come from the row inside the transaction;
	// ownershipGuard turns a transfer that landed since into 409.
	err = h.store.Update(ctx, id, &wf, ownershipGuard(cur.Scope, cur.OwnerID, workflowScope,
		func(prev *store.Workflow) error {
			wf.Scope, wf.OwnerID = prev.Scope, prev.OwnerID
			return nil
		}))
	if err != nil {
		return err
	}
	updated, err := h.store.Get(ctx, id)
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, updated)
	return nil
}

// Delete removes a workflow definition.
//
//	@Summary		Delete a workflow
//	@Description	Executions keep their snapshot, so past and in-flight runs still read and finish normally.
//	@Tags			workflows
//	@Param			id	path	string	true	"Workflow ID"
//	@Success		204
//	@Failure		404	{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/workflows/{id} [delete]
func (h *WorkflowHandler) Delete(c *gin.Context) error {
	// The workflow store's own delete: its triggers go with it.
	if err := deleteOwned(c, h.store.CrudStore, workflowScope, h.store.DeleteOwnedBy); err != nil {
		return err
	}
	c.Status(http.StatusNoContent)
	return nil
}

// SetScope promotes a workflow to global — after checking every step's agent
// is global too — or demotes it back to its author's private set.
//
//	@Summary	Change a workflow's scope
//	@Tags		workflows
//	@Accept		json
//	@Param		id		path	string		true	"Workflow ID"
//	@Param		scope	body	scopeReq	true	"global or private"
//	@Success	204
//	@Failure	400	{object}	ErrorResponse	"a promote holds non-global step agents"
//	@Failure	409	{object}	ErrorResponse	"name collision in the target scope"
//	@Security	BearerAuth
//	@Router		/workflows/{id}/scope [post]
func (h *WorkflowHandler) SetScope(c *gin.Context) error {
	return setScope(c, h.store.CrudStore, "workflow", workflowScope,
		func(wf *store.Workflow, target string) error {
			wf.Scope = target
			return h.validateStepAgents(c, wf)
		},
		h.store.SetScope)
}

// SetOwner transfers the workflow to another account (admin).
//
//	@Summary	Reassign a workflow's owner (admin)
//	@Tags		workflows
//	@Accept		json
//	@Param		id		path	string			true	"Workflow ID"
//	@Param		body	body	SetOwnerRequest	true	"The new owner"
//	@Success	204
//	@Failure	400	{object}	ErrorResponse	"malformed body, or no such user"
//	@Failure	409	{object}	ErrorResponse	"name collision in the target owner's namespace"
//	@Security	BearerAuth
//	@Router		/workflows/{id}/owner [put]
func (h *WorkflowHandler) SetOwner(c *gin.Context) error {
	return setOwner(c, func(ctx context.Context, id, userID string) error {
		wf, err := h.store.Get(ctx, id)
		if err != nil {
			return err
		}
		// Step agents are re-validated AS THE NEW OWNER — decisions §5.29.
		wf.OwnerID = userID
		if err := h.validateStepAgents(c, wf); err != nil {
			return err
		}
		return h.store.SetOwner(ctx, id, userID)
	})
}
