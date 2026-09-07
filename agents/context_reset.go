package agents

import (
	"context"
	"errors"
	"log/slog"

	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/tracing"
)

// ContextResetter is an optional Compactor capability: fold everything but
// the newest user message on request, carrying whatever the compactor keeps
// for the fresh context (spec §2.5i). compaction.Compactor implements it.
type ContextResetter interface {
	Reset(ctx context.Context, entries []session.Entry) ([]session.Entry, error)
}

// NewContextTool returns new_context: the model asks for a fresh context
// window, which the run grants at the turn's save point (spec §2.5i). Give
// it only to a run whose session can reset; elsewhere the request is
// recorded as ignored.
func NewContextTool() *Tool {
	return NewTool("new_context",
		"Start a new context window when the current one no longer helps. It takes effect when this turn ends: "+
			"everything but your memory and the user's latest message leaves your context, and history_search still finds it. "+
			"Write what you need to keep to memory first.",
		func(_ context.Context, tc *ToolContext, _ struct{}) (string, error) {
			tc.RequestContextReset()
			return "A new context window starts when this turn ends. Your memory and the last user message carry over; use history_search for anything else.", nil
		})
}

// resetContext performs a model-requested reset at the save point: a
// CompactionAware storage compacts with Reset set, a ContextResetter folds
// in memory. did reports that the context was rebuilt; a session that
// cannot reset ignores the request and says so in a diagnostic.
func (r *runner) resetContext(ctx context.Context) (input []InputItem, did bool, err error) {
	sess := r.opts.Conversation.Session
	if sess == nil {
		r.ignoreReset(ctx, "no session")
		return nil, false, nil
	}
	cur := session.Cursor{Limit: -session.ResolveLimit(r.opts.Conversation.Settings)}
	var span *tracing.SpanHandle
	startSpan := func() *tracing.SpanHandle {
		if span == nil {
			span = r.trace.StartCompactionSpan(r.agentParentID())
			span.Set("reset", true)
		}
		return span
	}
	defer func() {
		if span != nil {
			span.Finish()
		}
	}()

	var entries []session.Entry
	switch {
	case isCompactionAware(sess):
		cs := sess.Storage().(session.CompactionAware)
		cerr := cs.RunCompaction(ctx, session.CompactionArgs{
			Force: true, Reset: true,
			ResponseID:    r.lastResponseID,
			Store:         r.lastStore,
			OffChainItems: r.offChainItems(),
			StartSpan:     startSpan,
		})
		if cerr != nil {
			startSpan().SetError(cerr.Error(), nil)
			r.log.component("compaction").Warn(ctx, "context reset failed; continuing", slog.String("error", cerr.Error()))
			RecordDiagnostic(ctx, DiagCompactionFailed, cerr, map[string]any{"point": "reset"})
			return nil, false, nil
		}
		if entries, err = sess.ContextEntries(ctx, cur); err != nil {
			return nil, false, err
		}
	default:
		rs, ok := r.opts.Compaction.Compactor.(ContextResetter)
		if !ok {
			r.ignoreReset(ctx, "the session cannot reset")
			return nil, false, nil
		}
		startSpan()
		before, rerr := sess.ContextEntries(ctx, cur)
		if rerr != nil {
			return nil, false, rerr
		}
		if entries, rerr = rs.Reset(ctx, before); rerr != nil {
			span.SetError(rerr.Error(), nil)
			r.log.component("compaction").Warn(ctx, "context reset failed; continuing", slog.String("error", rerr.Error()))
			RecordDiagnostic(ctx, DiagCompactionFailed, rerr, map[string]any{"point": "reset"})
			return nil, false, nil
		}
	}
	history, err := session.ProjectEntries(entries, r.opts.Conversation.Projectors)
	if err != nil {
		return nil, false, err
	}
	r.log.component("compaction").Info(ctx, "context reset", slog.Int("entries_after", len(entries)))
	// Scrubbed like every other rebuild: a fold can orphan a tool output.
	return normalizeStoredInput(history), true, nil
}

// isCompactionAware reports whether the session's storage compacts itself.
func isCompactionAware(sess *session.Session) bool {
	_, ok := sess.Storage().(session.CompactionAware)
	return ok
}

// ignoreReset records a reset request the run could not honor.
func (r *runner) ignoreReset(ctx context.Context, reason string) {
	r.log.component("compaction").Warn(ctx, "context reset requested but ignored", slog.String("reason", reason))
	RecordDiagnostic(ctx, DiagContextResetIgnored, errors.New(reason), nil)
}
