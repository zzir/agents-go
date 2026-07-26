package agents

import (
	"context"
	"sync"
	"time"
)

// DiagnosticType names a kind of trouble a run survived.
//
// It is an open vocabulary: a consumer that meets a type it does not know shows
// it generically rather than failing, which is what lets the SDK report a new
// kind of trouble without a coordinated release downstream.
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

// Diagnostic records trouble a run went through and survived.
//
// It exists because the interesting failures are the ones that do NOT fail the
// run: three retries, a fallback to a slower model, a compaction pass that
// silently gave up. Those never reach an error return, so without this they
// live only in whatever log nobody kept — and "why was that answer bad" becomes
// unanswerable after the fact.
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

// NewDiagnostic builds a diagnostic from an error, classifying it.
//
// The timestamp is taken here rather than by the caller, so a diagnostic can
// never claim a time other than when the trouble happened.
func NewDiagnostic(t DiagnosticType, err error, details map[string]any) Diagnostic {
	d := Diagnostic{Type: t, Timestamp: time.Now().UTC(), Details: details}
	if err != nil {
		d.Code = CodeOf(err)
		d.Message = err.Error()
	}
	return d
}

// DiagnosticSink collects diagnostics. The runner installs one on the context
// for the duration of a run.
type DiagnosticSink struct {
	mu sync.Mutex
	ds []Diagnostic
}

// Record appends a diagnostic. Safe for concurrent use: tools run in parallel
// and each may report.
func (s *DiagnosticSink) Record(d Diagnostic) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.ds = append(s.ds, d)
	s.mu.Unlock()
}

// All returns a copy of what has been recorded.
func (s *DiagnosticSink) All() []Diagnostic {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Diagnostic(nil), s.ds...)
}

// TakeSince returns the diagnostics recorded after n, and the new count. It is
// how the runner attaches a turn's troubles to that turn's entries rather than
// to every entry in the run.
func (s *DiagnosticSink) TakeSince(n int) ([]Diagnostic, int) {
	if s == nil {
		return nil, n
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if n >= len(s.ds) {
		return nil, len(s.ds)
	}
	return append([]Diagnostic(nil), s.ds[n:]...), len(s.ds)
}

type diagnosticKey struct{}

// WithDiagnostics returns a context carrying sink, so code far from the runner
// — a model decorator, a custom tool — can report trouble it recovered from.
//
// The context is the channel because a Model receives one and nothing else that
// belongs to the run. A sink passed by field would need every decorator in the
// chain to forward it, and the one that forgot would silently swallow.
func WithDiagnostics(ctx context.Context, sink *DiagnosticSink) context.Context {
	return context.WithValue(ctx, diagnosticKey{}, sink)
}

// DiagnosticsFrom returns the sink on ctx, or nil.
func DiagnosticsFrom(ctx context.Context) *DiagnosticSink {
	s, _ := ctx.Value(diagnosticKey{}).(*DiagnosticSink)
	return s
}

// RecordDiagnostic reports trouble on whatever sink ctx carries. It is a no-op
// without one, so a call site never has to check.
func RecordDiagnostic(ctx context.Context, t DiagnosticType, err error, details map[string]any) {
	DiagnosticsFrom(ctx).Record(NewDiagnostic(t, err, details))
}

// attributeDiagnostics attaches the trouble seen since the last save to the
// batch's final entry.
//
// The last entry, so the diagnostics sit with the turn they describe rather
// than ahead of it — a retry that happened while producing this turn belongs to
// this turn, and the reader looking at "why is this answer bad" is looking at
// the answer, not at what preceded it.
func (r *runner) attributeDiagnostics(entries []SessionEntry) {
	if len(entries) == 0 {
		return
	}
	ds, n := r.diagnostics.TakeSince(r.diagnosticsSaved)
	r.diagnosticsSaved = n
	if len(ds) == 0 {
		return
	}
	entries[len(entries)-1].Diagnostics = ds
}
