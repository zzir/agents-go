package handler

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// Error codes carried in APIError.Code. Stable machine-readable identifiers;
// the message is human-readable detail.
const (
	CodeValidation = "validation"
	CodeNotFound   = "not_found"
	CodeConflict   = "conflict"
	CodeForbidden  = "forbidden"
	CodeUpstream   = "upstream"
	CodeInternal   = "internal"
)

// APIError is the machine-readable error payload of every non-2xx response.
type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ErrorResponse is the error envelope: {"error": {"code": ..., "message": ...}}.
type ErrorResponse struct {
	Error APIError `json:"error"`
}

func abortError(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, ErrorResponse{Error: APIError{Code: code, Message: message}})
}

// badRequest reports a 400 validation failure.
func badRequest(c *gin.Context, message string) {
	abortError(c, http.StatusBadRequest, CodeValidation, message)
}

// notFound reports a 404 for the requested resource.
func notFound(c *gin.Context) {
	abortError(c, http.StatusNotFound, CodeNotFound, "not found")
}

// conflict reports a 409: the request is well-formed but the resource is in
// the wrong state for it (e.g. listing tools of a disconnected MCP server).
func conflict(c *gin.Context, message string) {
	abortError(c, http.StatusConflict, CodeConflict, message)
}

// forbidden reports a 403 for operations disabled by server policy.
func forbidden(c *gin.Context, message string) {
	abortError(c, http.StatusForbidden, CodeForbidden, message)
}

// upstreamError reports a 502 for a failing upstream dependency (model
// provider, MCP server, sandbox host). The upstream message is forwarded —
// it's what the caller needs to fix the connection.
func upstreamError(c *gin.Context, err error) {
	abortError(c, http.StatusBadGateway, CodeUpstream, err.Error())
}

// internalError reports a 500. The error detail goes to the server log only.
func internalError(c *gin.Context, err error) {
	zerolog.Ctx(c.Request.Context()).Error().Err(err).
		Str("method", c.Request.Method).
		Str("path", c.FullPath()).
		Msg("internal error")
	abortError(c, http.StatusInternalServerError, CodeInternal, "internal error")
}

// storeError maps a store failure to a response: ErrNotFound → 404, anything
// else → 500.
func storeError(c *gin.Context, err error) {
	if errors.Is(err, store.ErrNotFound) {
		notFound(c)
		return
	}
	internalError(c, err)
}

// saveError maps a create/update store failure: a UNIQUE constraint violation
// → 409 (so uniqueness is enforced by the DB, not a racy handler pre-check),
// ErrNotFound → 404, anything else → 500. This centralizes the duplicate-key
// response so every table's uniqueness costs only its index.
func saveError(c *gin.Context, err error) {
	if cols, ok := store.UniqueViolation(err); ok {
		conflict(c, "already in use: "+cols)
		return
	}
	storeError(c, err)
}

// requireResource loads a parent resource by id before a sub-resource handler
// acts on it, so a missing parent is a 404 (via storeError) instead of a
// misleading downstream status (e.g. "not connected"). It returns false and has
// already written the error when the resource is missing or the lookup failed.
func requireResource[T any](c *gin.Context, get func(context.Context, string) (T, error), id string) bool {
	if _, err := get(c.Request.Context(), id); err != nil {
		storeError(c, err)
		return false
	}
	return true
}

// pageParams reads the backwards-pagination query parameters shared by the
// messages and traces listings: before_id (exclusive upper id bound) and
// limit (0 = unbounded). Invalid values read as 0.
func pageParams(c *gin.Context) (beforeID int64, limit int) {
	beforeID, _ = strconv.ParseInt(c.Query("before_id"), 10, 64)
	limit, _ = strconv.Atoi(c.Query("limit"))
	if beforeID < 0 {
		beforeID = 0
	}
	if limit < 0 {
		limit = 0
	}
	return beforeID, limit
}
