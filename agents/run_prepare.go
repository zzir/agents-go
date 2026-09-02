package agents

import (
	"cmp"
	"context"
	"log/slog"

	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/tracing"
)

// prepareRun builds the runner shared by Run and RunSync: it validates
// server-state options, wires the run context, starts (or joins) the trace and
// prepends session history to the model input. The returned finish func ends
// the trace — a no-op when the trace was joined (nested run) or tracing is off
// — and must be deferred by the caller. ResumeRun seeds its runner from a
// RunState instead, so it has its own entry construction and shares the loop
// plus observeRun.
//
// userInput is the run's new input, already normalized by runViaMiddleware —
// the one place a string or an []InputItem becomes an item list, so a
// middleware edits the very list the loop then uses.
func prepareRun(ctx context.Context, agent *Agent, userInput []InputItem, opts RunOptions) (*runner, []InputItem, func(), error) {
	maxTurns := opts.Exec.MaxTurns
	if maxTurns == 0 {
		maxTurns = DefaultMaxTurns
	}
	// A negative value (MaxTurnsUnlimited) disables the budget; it passes
	// through here and the turn check skips it.

	rc := NewRunContext(opts.Context)
	rc.inheritedOpts = &opts

	if err := validateServerState(opts); err != nil {
		return nil, nil, nil, err
	}

	r := &runner{opts: opts, rc: rc, maxTurns: maxTurns, userInput: userInput}
	finishTrace := r.observeRun(agent, false)

	// With a session, prepend stored history to the model input.
	modelInput := userInput
	if opts.Conversation.Session != nil {
		// Read the active branch minus what compaction folded, then project:
		// the projection renders each checkpoint's summary in the folded
		// history's place. An annotation or terminal entry is recorded but not
		// sent unless Conversation.Projectors says otherwise.
		limit := session.ResolveLimit(opts.Conversation.Settings)
		entries, herr := opts.Conversation.Session.ContextEntries(ctx, session.Cursor{Limit: -limit})
		if herr != nil {
			return nil, nil, nil, herr
		}
		// A read that came back FULL is one the window truncated: what it left
		// behind is stored, entered no request, and is on no response chain (see
		// offChainItems). Measuring it here (not "a window is configured") is what
		// lets the flag clear. A log exactly the window's size reads full too, so
		// this errs toward reporting — the safe direction.
		if limit > 0 && len(entries) >= limit {
			r.offChainHistory = true
		}
		// Compact before projecting: the compactor reasons about entries, and
		// projection turns whatever survives into model input.
		entries, _ = r.compactContext(ctx, CompactBeforeRun, entries)
		history, herr := session.ProjectEntries(entries, opts.Conversation.Projectors)
		if herr != nil {
			return nil, nil, nil, herr
		}
		// A projector that sends nothing for an item entry keeps it out of every
		// request while leaving it in the log — like what a window cut off (see
		// offChainItems).
		if withheldItemEntries(entries, opts.Conversation.Projectors) {
			r.offChainHistory = true
		}
		if len(history) > 0 {
			modelInput = make([]InputItem, 0, len(history)+len(userInput))
			modelInput = append(modelInput, history...)
			modelInput = append(modelInput, userInput...)
			// Scrub the merged history+input before it reaches the model: a
			// stored dangling tool call (e.g. persisted at an interruption) or
			// a duplicate re-sent item would otherwise 400 at the Responses API.
			modelInput = normalizeStoredInput(modelInput)
		}
	}

	return r, modelInput, finishTrace, nil
}

// observeRun wires everything a run is watched through before its first turn:
// the run logger, the diagnostics sink and the trace. The returned func ends
// the trace — a no-op when the trace was joined or tracing is off — and must be
// deferred by the caller.
//
// ResumeRun goes through here too, so a resumed run is observed exactly like the
// run it continues: resumed only marks the log and the trace name, and every
// other difference between the two would be a bug.
func (r *runner) observeRun(agent *Agent, resumed bool) func() {
	attrs := []slog.Attr{slog.String("agent", agent.Name)}
	if resumed {
		attrs = append(attrs, slog.Bool("resumed", true))
	}
	r.log = newRunLogger(r.opts.Log).component("run").with(attrs...)
	r.diagnostics = &DiagnosticSink{}

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

// loopSeed is the state the run loop starts from: fresh for a new run, carried
// state for a resume.
type loopSeed struct {
	agent         *Agent
	originalInput []InputItem
	rawResponses  []*ModelResponse

	// pendingResponse, on a resume, is the interrupted response the first
	// iteration re-processes instead of calling the model.
	pendingResponse *ModelResponse

	// cursor, on a resume, is the pause-time server-conversation cursor, so
	// the resumed run keeps sending deltas. Zero for a fresh run and for
	// locally-managed history.
	cursor serverCursor

	startTurn int
}

// seedLoop builds the loop's starting state. When resuming from an interruption
// it seeds prior state from the RunState and restores the persistence cursor and
// turn counter, so the resume continues from the pause instead of re-saving or
// resetting the turn budget.
func (r *runner) seedLoop(startAgent *Agent, originalInput []InputItem) loopSeed {
	seed := loopSeed{
		agent:         startAgent,
		originalInput: originalInput,
		rawResponses:  []*ModelResponse{},
		startTurn:     1,
	}
	if r.resume == nil {
		return seed
	}
	seed.agent = r.resume.CurrentAgent
	seed.originalInput = r.resume.OriginalInput
	seed.rawResponses = append([]*ModelResponse{}, r.resume.RawResponses...)
	seed.pendingResponse = r.resume.InterruptedResponse
	seed.cursor = r.resume.cursor
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
		seed.startTurn = r.resume.CurrentTurn
	}
	return seed
}

// validateServerState rejects incompatible server-managed conversation options.
// conversation_id and previous_response_id both put history on the server, so
// they cannot be combined with each other or with a local Session.
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
