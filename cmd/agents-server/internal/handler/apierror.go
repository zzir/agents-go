package handler

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// Aliases of the protocol error envelope, for the swagger annotations.
type (
	// APIError is the machine-readable error payload of every non-2xx response.
	APIError = protocol.APIError
	// ErrorResponse is the error envelope: {"error": {"code": ..., "message": ...}}.
	ErrorResponse = protocol.ErrorResponse
)

// apiError is a failure whose HTTP answer is already decided.
type apiError struct {
	status  int
	code    string
	message string
	cause   error // a 500's detail: logged, never sent
}

func (e *apiError) Error() string { return e.message }
func (e *apiError) Unwrap() error { return e.cause }

// errNotFound is the 404 of a resource that is absent or not the caller's to see.
var errNotFound = &apiError{status: http.StatusNotFound, code: protocol.CodeNotFound, message: "not found"}

// badRequest is a 400 validation failure.
func badRequest(message string) *apiError {
	return &apiError{status: http.StatusBadRequest, code: protocol.CodeValidation, message: message}
}

// unauthorized is a 401: no caller, or a credential that does not hold.
func unauthorized(message string) *apiError {
	return &apiError{status: http.StatusUnauthorized, code: protocol.CodeUnauthorized, message: message}
}

// forbidden is a 403: the caller may see the resource but not do this to it.
func forbidden(message string) *apiError {
	return &apiError{status: http.StatusForbidden, code: protocol.CodeForbidden, message: message}
}

// conflict is a 409: the request is well-formed but the resource is in the
// wrong state for it (e.g. listing tools of a disconnected MCP server).
func conflict(message string) *apiError {
	return &apiError{status: http.StatusConflict, code: protocol.CodeConflict, message: message}
}

// unavailable is a 503: the server is draining. Retryable, unlike a 500.
func unavailable(message string) *apiError {
	return &apiError{status: http.StatusServiceUnavailable, code: protocol.CodeUnavailable, message: message}
}

// upstream is a 502 for a failing dependency (model provider, MCP server,
// sandbox host), carrying its message.
func upstream(message string) *apiError {
	return &apiError{status: http.StatusBadGateway, code: protocol.CodeUpstream, message: message}
}

// classify decides the HTTP answer of a handler's failure — the one table
// every error goes through. An error nothing here names is a 500, its detail
// kept for the server log.
func classify(err error) *apiError {
	if e, ok := errors.AsType[*apiError](err); ok {
		return e
	}
	switch cols, unique := store.UniqueViolation(err); {
	case unique:
		return conflict("already in use: " + cols)
	case errors.Is(err, store.ErrProviderRef), errors.Is(err, store.ErrProviderScope):
		return badRequest(err.Error())
	case errors.Is(err, store.ErrSameScope), errors.Is(err, store.ErrOwnershipChanged),
		errors.Is(err, store.ErrGroupExists), errors.Is(err, store.ErrSandboxMoveDestination):
		return conflict(err.Error())
	case errors.Is(err, store.ErrNotFound):
		return errNotFound
	case errors.Is(err, store.ErrRevisionConflict): // the client re-reads and retries
		return conflict(err.Error())
	case store.IsMalformedID(err):
		return badRequest("malformed id")
	}
	return &apiError{status: http.StatusInternalServerError, code: protocol.CodeInternal, message: "internal error", cause: err}
}

// writeError answers a failure with its classified envelope and ends the chain.
func writeError(c *gin.Context, err error) {
	e := classify(err)
	if e.status == http.StatusInternalServerError {
		logging.Ctx(c.Request.Context()).Error("internal error", "error", err, "method", c.Request.Method, "path", c.FullPath())
	}
	c.AbortWithStatusJSON(e.status, protocol.NewErrorResponse(e.code, e.message))
}

// wrap adapts a handler that returns its failure to gin. It serves a gate as
// well: a nil return lets the chain continue.
func wrap(h func(c *gin.Context) error) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := h(c); err != nil {
			writeError(c, err)
		}
	}
}

// pageParams reads the backwards-pagination query parameters before_id
// (exclusive upper bound) and limit (absent or 0 = unbounded); a bad limit is 400.
func pageParams(c *gin.Context) (beforeID string, limit int, err error) {
	limit, err = queryInt(c, "limit")
	return c.Query("before_id"), limit, err
}

// queryInt reads a non-negative integer query parameter (absent = 0); an
// invalid value is 400.
func queryInt(c *gin.Context, name string) (int, error) {
	return nonNegativeInt(name, c.Query(name))
}

// nonNegativeInt parses raw as a non-negative integer (empty = 0); anything
// else is a 400 naming the parameter.
func nonNegativeInt(name, raw string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, badRequest(name + " must be a non-negative integer")
	}
	return n, nil
}

// maxNameLen caps a free-text name (a session's, an agent's) at bind time.
const maxNameLen = 256

// nameFits is a 400 for a name longer than maxNameLen.
func nameFits(name string) error {
	if utf8.RuneCountInString(name) > maxNameLen {
		return badRequest(fmt.Sprintf("name is longer than %d characters", maxNameLen))
	}
	return nil
}

// bindOptional binds a JSON body that may be absent: none is the zero request,
// anything else must parse or the answer is 400.
func bindOptional(c *gin.Context, req any) error {
	if err := c.ShouldBindJSON(req); err != nil && !errors.Is(err, io.EOF) {
		return badRequest(err.Error())
	}
	return nil
}

// created answers 201 with body and names id as the request's audit
// resource — a create has no path parameter to carry it.
func created(c *gin.Context, id string, body any) {
	server.SetAuditResource(c, id)
	c.JSON(http.StatusCreated, body)
}
