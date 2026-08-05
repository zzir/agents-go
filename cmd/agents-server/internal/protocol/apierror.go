package protocol

// APIError is the machine-readable error payload of every non-2xx REST
// response.
type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ErrorResponse is the REST error envelope: {"error": {"code": ..., "message": ...}}.
//
// It sits in this package for the same reason the WebSocket messages do — it is
// a frozen wire contract with two emitters. Besides the handler package, the
// server package writes it for auth failures and unmatched API paths, and
// server cannot import handler (handler depends on server.WSConn), so a second
// declaration there would drift the moment the envelope changes.
type ErrorResponse struct {
	Error APIError `json:"error"`
}

// NewErrorResponse builds the envelope for a code/message pair.
func NewErrorResponse(code, message string) ErrorResponse {
	return ErrorResponse{Error: APIError{Code: code, Message: message}}
}

// APIError.Code values, the vocabulary documented in README "Errors". Stable
// machine-readable identifiers; the message carries the human-readable detail.
//
// This is a namespace of its own, disjoint from the RunError codes in
// messages.go: these classify a REQUEST, those classify a RUN.
const (
	CodeValidation   = "validation"
	CodeUnauthorized = "unauthorized"
	CodeForbidden    = "forbidden"
	CodeNotFound     = "not_found"
	CodeConflict     = "conflict"
	CodeUpstream     = "upstream"
	CodeInternal     = "internal"
	// CodeUnavailable is a transient refusal — the server is shutting down.
	// Distinct from internal: the request was fine and retrying elsewhere (or
	// later) is the answer.
	CodeUnavailable = "unavailable"
)
