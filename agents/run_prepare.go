package agents

import (
	"cmp"
	"context"
	"log/slog"

	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/tracing"
)

// prepareRun builds the runner shared by Run and RunSync: it normalizes
// the input, validates server-state options, wires the run context, starts
// (or joins) the trace and prepends session history to the model input. The
// returned finish func ends the trace — a no-op when the trace was joined
// (nested run) or tracing is off — and must be deferred by the caller.
// ResumeRun seeds its runner from a RunState instead, so it has its own entry
// construction and shares the loop plus observeRun.
func prepareRun(ctx context.Context, agent *Agent, input any, opts RunOptions) (*runner, []InputItem, func(), error) {
	maxTurns := opts.Exec.MaxTurns
	if maxTurns == 0 {
		maxTurns = DefaultMaxTurns
	}
	// A negative value (MaxTurnsUnlimited) disables the budget; it passes
	// through here and the turn check skips it.

	rc := NewRunContext(opts.Context)
	rc.inheritedOpts = &opts

	userInput, err := normalizeInput(input)
	if err != nil {
		return nil, nil, nil, err
	}
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
		cur := session.Cursor{Limit: -session.ResolveLimit(opts.Conversation.Settings)}
		entries, herr := opts.Conversation.Session.ContextEntries(ctx, cur)
		if herr != nil {
			return nil, nil, nil, herr
		}
		// Compact before projecting: the compactor reasons about entries —
		// their kinds, their turns, their usage — and projection is what turns
		// whatever survives into model input.
		entries, _ = r.compactContext(ctx, CompactBeforeRun, entries)
		history, herr := session.ProjectEntries(entries, opts.Conversation.Projectors)
		if herr != nil {
			return nil, nil, nil, herr
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
// ResumeRun goes through here too, so a resumed run is observed exactly like
// the run it continues: resumed only marks the log and the trace name, and
// every other difference between the two is a bug (a group id that reached one
// trace but not the other left a paused/resumed pair unlinkable in exactly the
// view built to follow it).
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
	agent          *Agent
	originalInput  []InputItem
	generatedItems []*RunItem
	rawResponses   []*ModelResponse

	// pendingResponse, on a resume, is the interrupted response the first
	// iteration re-processes instead of calling the model.
	pendingResponse *ModelResponse

	// cursor, on a resume, is the pause-time server-conversation cursor, so
	// the resumed run keeps sending deltas. Zero for a fresh run and for
	// locally-managed history.
	cursor serverCursor

	startTurn int
}

// seedLoop builds the loop's starting state. When resuming from an
// interruption it seeds prior state from the RunState and restores the
// runner's persistence cursor: the interrupted run already persisted its user
// input and every turn up to the pause (holding back the pending, output-less
// tool calls), so the resume continues from that cursor instead of re-saving.
// Turn counting also continues where the interrupted run stopped, so repeated
// interrupt/resume cycles cannot exceed the turn budget.
func (r *runner) seedLoop(startAgent *Agent, originalInput []InputItem) loopSeed {
	seed := loopSeed{
		agent:          startAgent,
		originalInput:  originalInput,
		generatedItems: []*RunItem{},
		rawResponses:   []*ModelResponse{},
		startTurn:      1,
	}
	if r.resume == nil {
		return seed
	}
	seed.agent = r.resume.CurrentAgent
	seed.originalInput = r.resume.OriginalInput
	seed.generatedItems = append([]*RunItem{}, r.resume.GeneratedItems...)
	seed.rawResponses = append([]*ModelResponse{}, r.resume.RawResponses...)
	seed.pendingResponse = r.resume.InterruptedResponse
	seed.cursor = r.resume.cursor
	sessionSeed := r.resume.SessionItems
	if sessionSeed == nil {
		sessionSeed = r.resume.GeneratedItems
	}
	r.sessionItems = append([]*RunItem{}, sessionSeed...)
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
