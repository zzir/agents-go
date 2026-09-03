package handler

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// The error envelope is declared in internal/protocol (shared with the server
// package); these aliases are what the swagger annotations refer to.
type (
	// APIError is the machine-readable error payload of every non-2xx response.
	APIError = protocol.APIError
	// ErrorResponse is the error envelope: {"error": {"code": ..., "message": ...}}.
	ErrorResponse = protocol.ErrorResponse
)

func abortError(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, protocol.NewErrorResponse(code, message))
}

// badRequest reports a 400 validation failure.
func badRequest(c *gin.Context, message string) {
	abortError(c, http.StatusBadRequest, protocol.CodeValidation, message)
}

// notFound reports a 404 for the requested resource.
func notFound(c *gin.Context) {
	abortError(c, http.StatusNotFound, protocol.CodeNotFound, "not found")
}

// conflict reports a 409: the request is well-formed but the resource is in
// the wrong state for it (e.g. listing tools of a disconnected MCP server).
func conflict(c *gin.Context, message string) {
	abortError(c, http.StatusConflict, protocol.CodeConflict, message)
}

// unavailable reports a 503: the server is draining and cannot take the
// request. Retryable, unlike an internal error.
func unavailable(c *gin.Context, message string) {
	abortError(c, http.StatusServiceUnavailable, protocol.CodeUnavailable, message)
}

// upstreamError reports a 502 for a failing upstream dependency (model
// provider, MCP server, sandbox host), forwarding the upstream message.
func upstreamError(c *gin.Context, err error) {
	abortError(c, http.StatusBadGateway, protocol.CodeUpstream, err.Error())
}

// internalError reports a 500. The error detail goes to the server log only.
func internalError(c *gin.Context, err error) {
	logging.Ctx(c.Request.Context()).Error("internal error", "error", err, "method", c.Request.Method, "path", c.FullPath())
	abortError(c, http.StatusInternalServerError, protocol.CodeInternal, "internal error")
}

// storeError maps a store failure: ErrNotFound → 404, ErrRevisionConflict →
// 409 (the client re-reads and retries), anything else → 500.
func storeError(c *gin.Context, err error) {
	if errors.Is(err, store.ErrNotFound) {
		notFound(c)
		return
	}
	if errors.Is(err, store.ErrRevisionConflict) {
		conflict(c, err.Error())
		return
	}
	if store.IsMalformedID(err) {
		badRequest(c, "malformed id")
		return
	}
	internalError(c, err)
}

// badRequestError carries a 400's message out of a store callback (a
// handler's rule run inside the store's transaction). saveError maps it.
type badRequestError string

func (e badRequestError) Error() string { return string(e) }

// saveError maps a create/update store failure: UNIQUE violation → 409, a
// refused provider reference or badRequestError → 400, ErrNotFound → 404, else 500.
func saveError(c *gin.Context, err error) {
	if cols, ok := store.UniqueViolation(err); ok {
		conflict(c, "already in use: "+cols)
		return
	}
	_, rejected := errors.AsType[badRequestError](err)
	if errors.Is(err, store.ErrProviderRef) || errors.Is(err, store.ErrProviderScope) || rejected {
		badRequest(c, err.Error())
		return
	}
	if errors.Is(err, store.ErrSameScope) || errors.Is(err, store.ErrOwnershipChanged) || errors.Is(err, store.ErrGroupExists) ||
		errors.Is(err, store.ErrSandboxMoveDestination) {
		conflict(c, err.Error())
		return
	}
	storeError(c, err)
}

// pageParams reads the backwards-pagination query parameters before_id
// (exclusive upper bound) and limit (0 = unbounded); invalid values read as 0.
func pageParams(c *gin.Context) (beforeID string, limit int) {
	beforeID = c.Query("before_id")
	limit, _ = strconv.Atoi(c.Query("limit"))
	if limit < 0 {
		limit = 0
	}
	return beforeID, limit
}

// created answers 201 with body and names id as the request's audit
// resource — a create has no path parameter to carry it.
func created(c *gin.Context, id string, body any) {
	server.SetAuditResource(c, id)
	c.JSON(http.StatusCreated, body)
}
