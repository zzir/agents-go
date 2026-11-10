package agents

import (
	"cmp"
	"context"
	"log/slog"
	"slices"

	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/tracing"
)

// newRunner builds a fresh run's runner: validated options, run context, turn
// budget.
func newRunner(in RunInput) (*runner, error) {
	opts := *in.Opts
	if err := validateServerState(opts); err != nil {
		return nil, err
	}
	// A negative value (MaxTurnsUnlimited) disables the budget; it passes
	// through here and the turn check skips it.
	maxTurns := opts.Exec.MaxTurns
	if maxTurns == 0 {
		maxTurns = DefaultMaxTurns
	}
	rc := NewRunContext(opts.Context)
	rc.inheritedOpts = &opts
	return &runner{opts: opts, rc: rc, maxTurns: maxTurns, userInput: in.Input}, nil
}

// startInput is what the loop starts from: a resume's carried input, or the
// session's stored history ahead of the new input.
func (r *runner) startInput(ctx context.Context) ([]InputItem, error) {
	if r.resume != nil {
		return r.resume.OriginalInput, nil
	}
	sess := r.opts.Conversation.Session
	if sess == nil {
		return r.userInput, nil
	}
	// The active branch minus what compaction folded, projected per kind —
	// see spec §2.5b.
	entries, err := sess.ContextEntries(ctx, r.historyCursor())
	if err != nil {
		return nil, err
	}
	// Compact before projecting: the compactor reasons about entries, and
	// projection turns whatever survives into model input.
	entries, _ = r.compactContext(ctx, CompactBeforeRun, entries)
	history, err := session.ProjectEntries(entries, r.opts.Conversation.Projectors)
	if err != nil {
		return nil, err
	}
	if len(history) == 0 {
		return r.userInput, nil
	}
	// Scrub the merged history+input: a dangling call or a duplicate would
	// 400 — see spec §2.1b.
	return normalizeStoredInput(slices.Concat(history, r.userInput)), nil
}

// observeRun wires the run logger, diagnostics sink and trace before the first
// turn. The returned func closes the trace; a no-op for a joined or untraced run.
func (r *runner) observeRun(agent *Agent) func() {
	resumed := r.resume != nil
	attrs := []slog.Attr{slog.String("agent", agent.Name)}
	if resumed {
		attrs = append(attrs, slog.Bool("resumed", true))
	}
	r.log = newRunLogger(r.opts.Log).component("run").with(attrs...)
	r.diagnostics = &diagnosticSink{}

	finishTrace := func() {}
	switch {
	case r.opts.parentTrace != nil:
		// Nested run (agent-as-tool): record spans into the parent's trace
		// rather than starting an orphan root trace. The parent finishes it.
		r.trace = r.opts.parentTrace
	case r.opts.Observe.Tracer != nil:
		workflow := cmp.Or(agent.Name, "Agent workflow")
		if resumed {
			workflow += " (resumed)"
		}
		var topts []tracing.TraceOption
		if r.opts.Observe.TraceGroupID != "" {
			topts = append(topts, tracing.WithGroupID(r.opts.Observe.TraceGroupID))
		}
		if r.opts.Observe.TraceMetadata != nil {
			topts = append(topts, tracing.WithMetadata(r.opts.Observe.TraceMetadata))
		}
		r.trace = r.opts.Observe.Tracer.StartTrace(workflow, topts...)
		finishTrace = r.trace.Finish
	}
	r.rc.activeTrace = r.trace
	return finishTrace
}

// seedLoop builds the loop's starting turn state; a resume restores the items,
// the persistence cursor and the turn counter from its RunState.
func (r *runner) seedLoop(startAgent *Agent, originalInput []InputItem) *turnState {
	st := &turnState{
		agent:         startAgent,
		originalInput: originalInput,
		rawResponses:  []*ModelResponse{},
		startTurn:     1,
		runStartHooks: true,
	}
	if r.resume == nil {
		return st
	}
	st.rawResponses = append(st.rawResponses, r.resume.RawResponses...)
	st.pendingResponse = r.resume.InterruptedResponse
	st.cursor = r.resume.cursor
	// GeneratedItems is the tail of the log the model still sees; a state with
	// no SessionItems saw no filter, so the two are one.
	sessionSeed := r.resume.SessionItems
	if sessionSeed == nil {
		sessionSeed = r.resume.GeneratedItems
	}
	r.sessionItems = append([]*RunItem{}, sessionSeed...)
	r.generatedFrom = max(0, len(r.sessionItems)-len(r.resume.GeneratedItems))
	r.persistedSessionItems = r.resume.PersistedSessionItems
	r.userInputSaved = true
	if r.resume.CurrentTurn > 1 {
		st.startTurn = r.resume.CurrentTurn
	}
	return st
}

// validateServerState rejects incompatible server-managed conversation options:
// ConversationID and UsePreviousResponseID exclude each other and a local Session.
func validateServerState(opts RunOptions) error {
	if opts.Conversation.ConversationID != "" {
		if opts.Conversation.UsePreviousResponseID {
			return NewUserError("ConversationID cannot be combined with UsePreviousResponseID")
		}
		if opts.Conversation.Session != nil {
			return NewUserError("ConversationID cannot be combined with a local Session")
		}
	}
	if opts.Conversation.UsePreviousResponseID && opts.Conversation.Session != nil {
		return NewUserError("UsePreviousResponseID cannot be combined with a local Session")
	}
	return nil
}

// normalizeInput coerces a string or []InputItem into the input list.
func normalizeInput(input any) ([]InputItem, error) {
	switch v := input.(type) {
	case string:
		return InputItemsFromText(v), nil
	case []InputItem:
		return v, nil
	case nil:
		return nil, NewUserError("run input must not be nil")
	default:
		return nil, NewUserError("unsupported run input type %T (want string or []InputItem)", input)
	}
}
