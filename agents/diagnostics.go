package agents

import (
	"context"
	"sync"
	"time"

	"github.com/zzir/agents-go/agents/session"
)

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

// attributeDiagnostics attaches the trouble seen since the last save to the batch's
// final entry, so diagnostics sit with the turn they describe, not ahead of it.
func (r *runner) attributeDiagnostics(entries []session.Entry) {
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
