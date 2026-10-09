package agents

import (
	"context"
	"sync"
	"time"

	"github.com/zzir/agents-go/agents/session"
)

// DiagnosticSink collects diagnostics; the runner installs one on the context
// for a run.
type DiagnosticSink struct {
	mu sync.Mutex
	ds []Diagnostic
}

// Record appends a diagnostic. Safe for concurrent use.
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

// TakeSince returns the diagnostics recorded after n, and the new count.
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

// WithDiagnostics returns a context carrying sink, the channel through which a
// model decorator or a custom tool reports trouble it recovered from.
func WithDiagnostics(ctx context.Context, sink *DiagnosticSink) context.Context {
	return context.WithValue(ctx, diagnosticKey{}, sink)
}

// DiagnosticsFrom returns the sink on ctx, or nil.
func DiagnosticsFrom(ctx context.Context) *DiagnosticSink {
	s, _ := ctx.Value(diagnosticKey{}).(*DiagnosticSink)
	return s
}

// RecordDiagnostic reports trouble on whatever sink ctx carries; a no-op without one.
func RecordDiagnostic(ctx context.Context, t DiagnosticType, err error, details map[string]any) {
	DiagnosticsFrom(ctx).Record(NewDiagnostic(t, err, details))
}

// attributeDiagnostics attaches the trouble seen since the last save to the
// batch's final entry.
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

// NewDiagnostic builds a diagnostic from an error, classifying it and stamping it now.
func NewDiagnostic(t DiagnosticType, err error, details map[string]any) Diagnostic {
	d := Diagnostic{Type: t, Timestamp: time.Now().UTC(), Details: details}
	if err != nil {
		d.Code = CodeOf(err)
		d.Message = err.Error()
	}
	return d
}
