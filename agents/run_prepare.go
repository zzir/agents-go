package agents

import (
	"cmp"
	"context"
	"log/slog"

	"github.com/zzir/agents-go/tracing"
)

// prepareRun builds the runner shared by Run and RunSync: it normalizes
// the input, validates server-state options, wires the run context, starts
// (or joins) the trace and prepends session history to the model input. The
// returned finish func ends the trace — a no-op when the trace was joined
// (nested run) or tracing is off — and must be deferred by the caller.
// ResumeRun has its own entry construction (it seeds from a RunState) and
// shares only the loop.
func prepareRun(ctx context.Context, agent *Agent, input any, opts RunOptions) (*runner, []TResponseInputItem, func(), error) {
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
	r.log = newRunLogger(opts.Log).component("run").with(slog.String("agent", agent.Name))
	r.diagnostics = &DiagnosticSink{}

	finishTrace := func() {}
	if opts.parentTrace != nil {
		// Nested run (agent-as-tool): record spans into the parent's trace
		// rather than starting an orphan root trace. The parent finishes it.
		r.trace = opts.parentTrace
	} else if opts.Observe.Tracer != nil {
		workflow := agent.Name
		workflow = cmp.Or(workflow, "Agent workflow")
		var topts []tracing.TraceOption
		if opts.Observe.TraceGroupID != "" {
			topts = append(topts, tracing.WithGroupID(opts.Observe.TraceGroupID))
		}
		if opts.Observe.TraceMetadata != nil {
			topts = append(topts, tracing.WithMetadata(opts.Observe.TraceMetadata))
		}
		r.trace = opts.Observe.Tracer.StartTrace(workflow, topts...)
		finishTrace = r.trace.Finish
	}
	rc.activeTrace = r.trace

	// With a session, prepend stored history to the model input.
	modelInput := userInput
	if opts.Conversation.Session != nil {
		// Read the active branch minus what compaction folded, then project:
		// the projection renders each checkpoint's summary in the folded
		// history's place. An annotation or terminal entry is recorded but not
		// sent unless Conversation.Projectors says otherwise.
		cur := Cursor{Limit: -resolveSessionLimit(opts.Conversation.Settings)}
		entries, herr := opts.Conversation.Session.ContextEntries(ctx, cur)
		if herr != nil {
			return nil, nil, nil, herr
		}
		// Compact before projecting: the compactor reasons about entries —
		// their kinds, their turns, their usage — and projection is what turns
		// whatever survives into model input.
		entries, _ = r.compactContext(ctx, CompactBeforeRun, entries)
		history, herr := ProjectEntries(entries, opts.Conversation.Projectors)
		if herr != nil {
			return nil, nil, nil, herr
		}
		if len(history) > 0 {
			modelInput = make([]TResponseInputItem, 0, len(history)+len(userInput))
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

// loopSeed is the state the run loop starts from: fresh for a new run, carried
// state for a resume.
type loopSeed struct {
	agent          *Agent
	originalInput  []TResponseInputItem
	generatedItems []RunItem
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
func (r *runner) seedLoop(startAgent *Agent, originalInput []TResponseInputItem) loopSeed {
	seed := loopSeed{
		agent:          startAgent,
		originalInput:  originalInput,
		generatedItems: []RunItem{},
		rawResponses:   []*ModelResponse{},
		startTurn:      1,
	}
	if r.resume == nil {
		return seed
	}
	seed.agent = r.resume.CurrentAgent
	seed.originalInput = r.resume.OriginalInput
	seed.generatedItems = append([]RunItem{}, r.resume.GeneratedItems...)
	seed.rawResponses = append([]*ModelResponse{}, r.resume.RawResponses...)
	seed.pendingResponse = r.resume.InterruptedResponse
	seed.cursor = r.resume.cursor
	sessionSeed := r.resume.SessionItems
	if sessionSeed == nil {
		sessionSeed = r.resume.GeneratedItems
	}
	r.sessionItems = append([]RunItem{}, sessionSeed...)
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

// normalizeInput coerces a string or []TResponseInputItem into the input list.
func normalizeInput(input any) ([]TResponseInputItem, error) {
	switch v := input.(type) {
	case string:
		return InputItemsFromText(v), nil
	case []TResponseInputItem:
		return v, nil
	case nil:
		return nil, NewUserError("run input must not be nil")
	default:
		return nil, NewUserError("unsupported run input type %T (want string or []TResponseInputItem)", input)
	}
}
