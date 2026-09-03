package session

import "time"

// DiagnosticType names a kind of trouble a run survived. An open vocabulary:
// an unknown type is shown generically, never a failure.
type DiagnosticType string

const (
	// DiagModelRetry is a model call that failed and was retried.
	DiagModelRetry DiagnosticType = "model_retry"
	// DiagModelFallback is a switch to a backup model or provider.
	DiagModelFallback DiagnosticType = "model_fallback"
	// DiagStreamError is a streaming call that broke mid-response.
	DiagStreamError DiagnosticType = "stream_error"
	// DiagToolPanic is a tool that panicked and was recovered.
	DiagToolPanic DiagnosticType = "tool_panic"
	// DiagToolTimeout is a tool that hit its deadline.
	DiagToolTimeout DiagnosticType = "tool_timeout"
	// DiagCompactionFailed is a compaction pass that failed; the run continued
	// with the context it had.
	DiagCompactionFailed DiagnosticType = "compaction_failed"
	// DiagResponseTruncated is a model response cut off at the output-token
	// limit, whose tool calls were refused.
	DiagResponseTruncated DiagnosticType = "response_truncated"
	// DiagContextOverflow is a model call that failed because the context did
	// not fit, after which the run compacted and tried again.
	DiagContextOverflow DiagnosticType = "context_overflow"
)

// Diagnostic records trouble a run went through and survived — retries, a
// fallback model, a compaction that gave up: what never reaches an error return.
type Diagnostic struct {
	Type      DiagnosticType `json:"type"`
	Timestamp time.Time      `json:"timestamp"`
	// Code classifies the underlying error, when there was one.
	Code ErrorCode `json:"code,omitzero"`
	// Message is a one-line human-facing summary.
	Message string `json:"message,omitzero"`
	// Details carries type-specific fields (attempt number, model name, …).
	Details map[string]any `json:"details,omitzero"`
}
