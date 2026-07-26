package agents

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/zzir/agents-go/tracing"
)

// DefaultMaxTurns is the turn budget applied when RunOptions.Exec.MaxTurns is zero.
const DefaultMaxTurns = 10

// MaxTurnsUnlimited disables the turn budget when set as RunOptions.Exec.MaxTurns —
// the run loops until it produces a final output, hands off to a finishing
// agent, or is cancelled. The counterpart of Python's max_turns=None. Use with
// care: a model that never finishes will loop indefinitely.
const MaxTurnsUnlimited = -1

// ModelInputData is the editable portion of a model call passed to a
// CallModelInputFilter: the system instructions and the input items.
type ModelInputData struct {
	Instructions string
	Input        []TResponseInputItem
}

// CallModelInputFilter edits the instructions and input items just before a
// model call. Returning an error aborts the run.
type CallModelInputFilter func(ctx context.Context, rc *RunContext, agent *Agent, data ModelInputData) (ModelInputData, error)

// RunOptions configures a run. The zero value is valid: MaxTurns defaults to
// DefaultMaxTurns and a fresh RunContext is created. A ModelProvider (or an
// agent with an explicit ModelImpl) is required so the runner can obtain a Model.
// RunOptions configures a run. The zero value is usable: agents.RunSync(ctx,
// agent, "hi", agents.RunOptions{}) works as long as the agent can resolve a
// model.
//
// Fields are grouped by what they configure rather than listed flat. The groups
// are not cosmetic — Conversation in particular collects options that constrain
// each other (a local Session and server-managed state are alternatives, not
// layers), which a flat list hid.
type RunOptions struct {
	// Model selects and configures the model behind every agent in the run.
	Model ModelOptions

	// Conversation decides where history lives and how it is combined with the
	// run's new input.
	Conversation ConversationOptions

	// Exec bounds and steers the loop itself.
	Exec ExecOptions

	// Compaction shrinks the model's context as the conversation grows. The
	// zero value disables it.
	Compaction CompactionOptions

	// Guardrails apply to the whole run, in addition to each agent's own
	// Agent.Guardrails. Run-level guardrails are consulted first at every stage.
	Guardrails []Guardrail

	// Middlewares wrap the run, outermost first. They are where optional
	// policy lives — logging, retrying, recovering — so the loop does not grow
	// a field and a branch for each one.
	Middlewares []RunMiddleware

	// Observe configures tracing.
	Observe ObserveOptions

	// Log configures the SDK's own structured logging. The zero value is
	// silent.
	Log LogConfig

	// Context is arbitrary user data threaded through tools, guardrails and
	// hooks via RunContext.Context. Ignored if RunContext is set.
	Context any

	// RunContext, when set, is used directly (and its Usage is accumulated into).
	// Otherwise a new one wrapping Context is created.
	RunContext *RunContext

	// parentTrace, when set, makes the run record its spans into an existing
	// trace instead of starting (and finishing) its own. Set internally for
	// nested agent-as-tool runs; not user-facing.
	parentTrace *tracing.TraceHandle

	// parentSpanID, when set alongside parentTrace, parents the nested run's
	// agent spans under the function span of the agent-as-tool call that
	// triggered it, so trace trees show which tool call owns the nested run.
	// Set internally; not user-facing.
	parentSpanID string
}

// ModelOptions selects and configures the model used by every agent in a run.
type ModelOptions struct {

	// ModelProvider resolves an agent's model name to a Model. Required unless
	// every agent in the run sets ModelImpl (or Override below is set).
	Provider ModelProvider

	// Override replaces the model for every agent in the run, ignoring agent
	// model names. Takes precedence over Provider lookups.
	Override Model

	// ModelSettings is a run-level settings override merged over each agent's
	// own ModelSettings.
	Settings *ModelSettings

	// CallModelInputFilter, when set, is invoked just before each model call to
	// edit the instructions and input items sent (e.g. to trim tokens or inject
	// context). It does not change what is saved to the session.
	InputFilter CallModelInputFilter
}

// ConversationOptions decides where a run's history lives: in a local Session,
// or on the server via previous_response_id or a conversation id. The two are
// alternatives — a run that sets both is rejected.
type ConversationOptions struct {

	// Session, when set, supplies and persists conversation history: prior items
	// are prepended to the input, and the new input plus generated items are
	// saved after the run completes.
	Session *Session

	// SessionInputCallback customizes how stored session history is combined with
	// the run's new input. Nil (the default) appends new input to history; a
	// custom callback may reorder, filter or fold history. Only genuinely new
	// items are persisted back to the session. Ignored without a Session — the
	// counterpart of Python's RunConfig.session_input_callback.
	InputCallback SessionInputCallback

	// SessionSettings overrides how the run reads the Session (e.g. how many
	// recent items to load). Non-zero fields take precedence over a Session-level
	// default. Ignored without a Session — the counterpart of Python's
	// RunConfig.session_settings.
	Settings *SessionSettings

	// UsePreviousResponseID opts into server-managed conversation state: instead
	// of resending the full history each turn, the runner chains calls via the
	// OpenAI Responses API's previous_response_id and sends only new items. This
	// saves tokens but requires a model that returns response IDs and keeps
	// responses stored (the default; do not set ModelSettings.Store=false).
	UsePreviousResponseID bool

	// ConversationID attaches the run to a server-side OpenAI conversation
	// (the Responses API `conversation` parameter). Like UsePreviousResponseID,
	// the server holds history, so the runner sends only new items each turn. It
	// is server-managed state and must not be combined with a local Session.
	ConversationID string

	// Projectors overrides how session entries become model input, per entry
	// kind. It is the single place that answers "what does the model get to
	// read": the defaults send items and compaction checkpoints and nothing
	// else, so an annotation or terminal output is recorded without being put
	// in the model's mouth.
	//
	// A projector mapped to nil suppresses that kind entirely. The common
	// override is the opposite — projecting EntryKindTerminal as a user message
	// so the model can see what was run by hand.
	Projectors map[EntryKind]EntryProjector
}

// ExecOptions bounds and steers the run loop.
type ExecOptions struct {
	// MaxTurns bounds the number of model calls before the run aborts with a
	// MaxTurnsError. Zero means DefaultMaxTurns.
	MaxTurns int

	// MaxToolConcurrency bounds how many function tools run concurrently within a
	// single turn. Zero means no limit (every tool call in the turn runs in
	// parallel).
	MaxToolConcurrency int

	// ToolNotFoundBehavior controls what happens when the model calls a tool the
	// agent does not expose. The default (ToolNotFoundError) aborts the run;
	// ToolNotFoundReturnToModel feeds an error back so the model can retry.
	ToolNotFoundBehavior ToolNotFoundBehavior

	// PreApprovalToolInputGuardrails, when true, runs a tool's input guardrails
	// before surfacing a human-approval interruption for it: a guardrail
	// rejection returns the guardrail message as the tool output without
	// emitting an approval request or executing the tool. Calls that pass still
	// re-run the same guardrails immediately before execution after approval,
	// so time-sensitive checks are revalidated on resume. Off by default —
	// the counterpart of Python's
	// RunConfig.tool_execution.pre_approval_tool_input_guardrails.
	PreApprovalToolInputGuardrails bool

	// HandoffInputFilter is a run-level default applied to any handoff that does
	// not set its own Handoff.InputFilter. Use NestHandoffHistory to fold prior
	// history across all handoffs.
	HandoffInputFilter func(HandoffInputData) HandoffInputData

	// ErrorHandlers supplies per-error-kind recovery handlers that can turn a
	// failing run — max turns exceeded, a model refusal, or an invalid
	// structured final output — into a normal completion with a fallback final
	// output. The zero value leaves every error fatal. The counterpart of
	// Python's Runner.run(..., error_handlers={...}).
	ErrorHandlers RunErrorHandlers

	// ToolLoop bounds the tool loop: consecutive all-failed turns, and what
	// happens when the turn budget runs out. The zero value is sensible.
	ToolLoop ToolLoopPolicy

	// ReasoningItemIDPolicy controls whether reasoning-item ids are kept when run
	// items are converted back into model input on later turns. The default
	// (ReasoningItemIDPreserve) keeps them; ReasoningItemIDOmit strips them. It is
	// persisted across interruptions in RunState — the counterpart of Python's
	// RunConfig.reasoning_item_id_policy.
	ReasoningItemIDPolicy ReasoningItemIDPolicy

	// PrepareNextTurn rebuilds the next turn's configuration at the turn
	// boundary, returning nil to leave it to the usual resolution.
	//
	// It is how a run changes shape mid-flight — swap in a cheaper model once
	// the hard part is done, withdraw a tool after it has been used, tighten
	// the instructions — without mutating the Agent, which a concurrent run
	// may be reading.
	PrepareNextTurn func(ctx context.Context, tr *TurnResult) (*TurnSnapshot, error)

	// ShouldStopAfterTurn ends the run after a turn that would otherwise
	// continue, instead of calling the model again.
	//
	// It is consulted at the turn boundary — after the turn's items are
	// persisted, before the next model call — so a run stopped here has its
	// full history saved. It is a predicate, not a producer: the final output
	// is the turn's last message text, or the last tool output when the turn
	// produced no message. Anything richer is available on the RunResult.
	//
	// It replaces the agent-level tool-use behavior it grew out of. Deciding
	// from what a turn produced is strictly more expressive than naming tools
	// up front, and it belongs to the run rather than the agent: the same agent
	// is reused across runs that want to stop at different points.
	ShouldStopAfterTurn func(ctx context.Context, tr *TurnResult) (bool, error)
}

// ObserveOptions configures tracing for a run.
type ObserveOptions struct {

	// Tracer, when set, records a trace of the run with a span per model call.
	// Build one with tracing.NewTracer(processor).
	Tracer *tracing.Tracer

	// TraceIncludeSensitiveData controls whether generation spans record the
	// full model request (model, system instructions, input items) and output
	// items. nil reads the OPENAI_AGENTS_TRACE_INCLUDE_SENSITIVE_DATA
	// environment variable, where anything but "false" means true — matching
	// the Python SDK's RunConfig.trace_include_sensitive_data default. Set to
	// false when trace exports must not carry conversation content.
	IncludeSensitiveData *bool

	// TraceGroupID links this run's trace to a group of related traces (e.g.
	// one chat thread across several runs) — the counterpart of Python's
	// RunConfig.group_id. Only used when Tracer starts a new trace.
	TraceGroupID string

	// TraceMetadata attaches user metadata to the run's trace — the
	// counterpart of Python's RunConfig.trace_metadata. Only used when Tracer
	// starts a new trace.
	TraceMetadata map[string]any
}

// Run starts an agent run and returns it as a stream plus a control handle.
// Nothing executes until the stream is ranged: the run happens on the
// consumer's goroutine, one event at a time.
//
// input may be a string or a []TResponseInputItem; use InputItemsFromText for
// the common single-message case.
//
//	stream, ctrl := agents.Run(ctx, agent, "hi", agents.RunOptions{})
//	for ev, err := range stream {
//	    if err != nil { return err }
//	    ...
//	}
//
// For the result and nothing else, use RunSync — it also skips streaming the
// model call, which Run needs in order to forward raw events.
//
// Abandoning the stream stops the run where it stands. To stop cleanly at a
// turn boundary, call ctrl.StopAfterTurn and keep ranging until the stream
// ends.
func Run(ctx context.Context, agent *Agent, input any, opts RunOptions) (RunStream, RunControl) {
	ctrl := newRunControl()
	return withMiddleware(ctx, agent, input, opts, ctrl, true), ctrl
}

// RunSync executes a run to completion and returns its result. It is Run
// without the stream: the model is called without streaming, and no raw model
// events are produced.
//
// It is the entry point to reach for unless you need to observe a run as it
// happens.
func RunSync(ctx context.Context, agent *Agent, input any, opts RunOptions) (*RunResult, error) {
	ctrl := newRunControl()
	return withMiddleware(ctx, agent, input, opts, ctrl, false).Collect()
}

// withMiddleware builds the run's stream through the configured middleware
// chain. Input normalization happens once, up front, so a middleware inspects
// and edits the same item list the loop will use rather than a string it would
// have to normalize itself.
func withMiddleware(ctx context.Context, agent *Agent, input any, opts RunOptions, ctrl *runControl, rawEvents bool) RunStream {
	base := func(ctx context.Context, in RunInput) RunStream {
		return func(yield func(StreamEvent, error) bool) {
			runStream(ctx, in.Agent, in.Input, *in.Opts, ctrl, rawEvents, yield)
		}
	}
	if len(opts.Middlewares) == 0 {
		return func(yield func(StreamEvent, error) bool) {
			runStream(ctx, agent, input, opts, ctrl, rawEvents, yield)
		}
	}

	return func(yield func(StreamEvent, error) bool) {
		items, err := normalizeInput(input)
		if err != nil {
			yield(nil, err)
			return
		}
		in := RunInput{Agent: agent, Input: items, Opts: &opts}
		for ev, ierr := range chainMiddleware(base, opts.Middlewares)(ctx, in) {
			if !yield(ev, ierr) {
				return
			}
		}
	}
}

// runStream is the body shared by Run and RunSync: prepare, loop, and report
// the outcome as the stream's terminal event or terminal error.
func runStream(ctx context.Context, agent *Agent, input any, opts RunOptions, ctrl *runControl, rawEvents bool, yield func(StreamEvent, error) bool) {
	r, modelInput, finishTrace, err := prepareRun(ctx, agent, input, opts)
	if err != nil {
		yield(nil, err)
		return
	}
	defer finishTrace()
	r.yield = yield
	r.ctrl = ctrl
	r.rawEvents = rawEvents

	// The sink rides on the context so code far from the loop — a model
	// decorator, a custom tool — can report trouble it recovered from.
	ctx = WithDiagnostics(ctx, r.diagnostics)
	res, err := r.loop(ctx, agent, modelInput)
	r.finishStream(res, err)
}

// finishStream reports a completed loop to the consumer. A consumer that
// already stopped is told nothing further — yield has returned false, so there
// is nobody listening.
func (r *runner) finishStream(res *RunResult, err error) {
	r.ctrl.setPhase(PhaseIdle)
	if r.consumerStopped || errors.Is(err, errConsumerStopped) {
		return
	}
	if err != nil {
		r.yield(nil, err)
		return
	}
	r.yield(&RunCompletedEvent{Result: res}, nil)
}

// prepareRun builds the runner shared by Run and RunStreamed: it normalizes
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

	rc := opts.RunContext
	if rc == nil {
		rc = NewRunContext(opts.Context)
	}
	if rc.Usage == nil {
		rc.Usage = NewUsage()
	}
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
		if workflow == "" {
			workflow = "Agent workflow"
		}
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

	// With a session, prepend stored history to the model input. A
	// SessionInputCallback may instead reorder or fold history; when it does,
	// only the genuinely new items are persisted (r.userInput is narrowed).
	modelInput := userInput
	if opts.Conversation.Session != nil {
		// Read from the most recent compaction checkpoint onward, then project:
		// a checkpoint already represents everything before it, and projection
		// decides what the model reads. An annotation or terminal entry is
		// recorded but not sent unless Conversation.Projectors says otherwise.
		cur := Cursor{Limit: -resolveSessionLimit(opts.Conversation.Settings)}
		entries, herr := opts.Conversation.Session.ContextEntries(ctx, cur)
		if herr != nil {
			return nil, nil, nil, herr
		}
		// Compact before projecting: the compactor reasons about entries —
		// their kinds, their turns, their usage — and projection is what turns
		// whatever survives into model input.
		entries = r.compactContext(ctx, CompactBeforeRun, entries)
		history, herr := ProjectEntries(entries, opts.Conversation.Projectors)
		if herr != nil {
			return nil, nil, nil, herr
		}
		if opts.Conversation.InputCallback != nil {
			combined, cerr := opts.Conversation.InputCallback(history, userInput)
			if cerr != nil {
				return nil, nil, nil, cerr
			}
			// Persistence diffs against the raw combined so the callback's chosen
			// new items are saved intact, but the model input is scrubbed just like
			// the default branch below: a callback that folds history can carry a
			// dangling tool call or a duplicate that would otherwise 400 at the
			// Responses API.
			r.userInput = sessionAppendedItems(history, userInput, combined)
			modelInput = normalizeStoredInput(combined)
		} else if len(history) > 0 {
			modelInput = make([]TResponseInputItem, 0, len(history)+len(userInput))
			modelInput = append(modelInput, history...)
			modelInput = append(modelInput, userInput...)
			// Scrub the merged history+input before it reaches the model: a
			// stored dangling tool call (e.g. persisted by the Python SDK at an
			// interruption) or a duplicate re-sent item would otherwise 400 at
			// the Responses API. Mirrors Python's prepare_input_with_session.
			modelInput = normalizeStoredInput(modelInput)
		}
	}

	return r, modelInput, finishTrace, nil
}

// modelCallOutcome carries a model call's result off a goroutine when the call
// races the first-turn input guardrails (see the loop's guardCh path).
type modelCallOutcome struct {
	resp *ModelResponse
	err  error
}

// inputGuardOutcome carries the parallel input guardrails' collected results and
// tripwire/error off their goroutine, so the main loop can both honor the
// tripwire and record every result on the RunResult.
type inputGuardOutcome struct {
	results []GuardrailResult
	err     error
}

// runner holds the mutable state for a single Run invocation.
type runner struct {
	opts      RunOptions
	rc        *RunContext
	maxTurns  int
	userInput []TResponseInputItem // the new input to persist to the session
	resume    *RunState            // non-nil when resuming from an interruption
	trace     *tracing.TraceHandle // non-nil when tracing is enabled
	agentSpan *tracing.SpanHandle  // current agent span, parent of generation/tool spans

	// yield delivers events to the consumer ranging the RunStream. It is
	// always set for a run started through Run or RunSync — there is no
	// "non-streaming mode" any more, only a consumer that discards.
	//
	// It returns false once the consumer stops ranging; emit records that in
	// consumerStopped and the loop unwinds through errConsumerStopped.
	yield func(StreamEvent, error) bool

	// ctrl is the handle the caller got back from Run: the graceful-stop flag,
	// the phase indicator, and the live agent/turn.
	ctrl *runControl

	// rawEvents asks for the model to be called through StreamResponse so its
	// raw events reach the consumer. RunSync leaves it false and gets a single
	// GetResponse call instead.
	//
	// This is the ONE remaining difference between the two entry points. It
	// used to be six, all keyed off a nil check on the streaming handle.
	rawEvents bool

	// consumerStopped is set by emit when the consumer stopped ranging. The
	// loop checks it wherever it checks for cancellation.
	consumerStopped bool

	// sessionItems accumulates every generated item for session persistence.
	// Unlike the loop's generatedItems it is never reset by a handoff input
	// filter, so the session keeps the full conversation.
	sessionItems []RunItem

	// persistedSessionItems counts how many leading sessionItems have already
	// been written to the session. The loop persists incrementally — after each
	// turn and at an interruption — so a cancelled or failed run keeps every
	// completed turn instead of losing the whole run (matching Python's per-turn
	// save_result_to_session). Carried across interrupt/resume in RunState.
	persistedSessionItems int

	// userInputSaved guards the one-time persistence of userInput at loop start
	// so a per-turn save never rewrites it.
	userInputSaved bool

	// emitMu serializes yields. The run loop is not the only emitter: a tool
	// pushing progress does it from its own goroutine, and several tools run at
	// once.
	emitMu sync.Mutex

	// diagnostics collects trouble the run survived. Never nil.
	diagnostics *DiagnosticSink
	// diagnosticsSaved is how many diagnostics have already been attached to
	// entries, so each is recorded on the turn it happened in rather than on
	// every turn from then on.
	diagnosticsSaved int

	// log is the run's logger, already tagged with the run's identity. Never
	// nil — a disabled logger is a no-op rather than a check at every site.
	log *runLogger

	// lastUsage is the most recent model response's usage, held so the entries
	// that response produced can carry it.
	lastUsage *Usage

	// usagePending marks a model response whose usage has not yet landed on an
	// entry. It is cleared on attribution, so a turn persisted in two batches —
	// the split an approval pause creates — cannot count the same request
	// twice.
	usagePending bool

	// consecutiveErrorTurns counts turns in a row where every tool call failed.
	// A turn with any success clears it; ToolLoopPolicy decides when enough is
	// enough.
	consecutiveErrorTurns int

	// toolsUsedBy tracks which agents have called tools this run, driving the
	// tool_choice reset (Agent.DisableToolChoiceReset). Keyed by agent name so
	// it can be carried across an interrupt/resume in RunState (Python
	// serializes its tool-use tracker snapshot), keeping the reset in effect for
	// every agent that used tools before the pause — not just the interrupted
	// one.
	toolsUsedBy map[string]bool

	// lastResponseID / lastStore record the final model call's response id and
	// store setting, used to drive session compaction after persistence.
	lastResponseID string
	lastStore      *bool

	// guardrailMu guards guardrailResults: the tool stages record from the
	// concurrent per-tool-call goroutines in runFunctionTools, while the input
	// and output stages record from the main loop.
	guardrailMu      sync.Mutex
	guardrailResults []GuardrailResult
}

// recordGuardrailResults appends guardrail results under the lock, since
// concurrent tool calls record in parallel.
func (r *runner) recordGuardrailResults(res ...GuardrailResult) {
	if len(res) == 0 {
		return
	}
	r.guardrailMu.Lock()
	r.guardrailResults = append(r.guardrailResults, res...)
	r.guardrailMu.Unlock()
}

// snapshotGuardrailResults copies the accumulated results for a RunResult.
func (r *runner) snapshotGuardrailResults() []GuardrailResult {
	r.guardrailMu.Lock()
	defer r.guardrailMu.Unlock()
	return append([]GuardrailResult(nil), r.guardrailResults...)
}

// runGuardrails is the run-level guardrail set for an agent: run-scoped ones
// first, then the agent's own.
func (r *runner) runGuardrails(agent *Agent) []Guardrail {
	if len(r.opts.Guardrails) == 0 {
		return agent.Guardrails
	}
	out := make([]Guardrail, 0, len(r.opts.Guardrails)+len(agent.Guardrails))
	out = append(out, r.opts.Guardrails...)
	out = append(out, agent.Guardrails...)
	return out
}

// agentParentID returns the current agent span's ID for nesting child spans.
func (r *runner) agentParentID() string {
	if r.agentSpan == nil || r.agentSpan.Span == nil {
		return ""
	}
	return r.agentSpan.Span.SpanID
}

func (r *runner) loop(ctx context.Context, startAgent *Agent, originalInput []TResponseInputItem) (*RunResult, error) {
	// Finish the active agent span when the loop ends (nil-safe when untraced).
	defer func() { r.agentSpan.Finish() }()

	// cancelInputGuardrails, when set, cancels the in-flight input-guardrail
	// goroutine. Deferred so every early exit (e.g. a failed model call) stops
	// an LLM-based guardrail instead of letting it run to completion after the
	// run has already returned.
	var cancelInputGuardrails context.CancelFunc
	defer func() {
		if cancelInputGuardrails != nil {
			cancelInputGuardrails()
		}
	}()

	currentAgent := startAgent
	generatedItems := []RunItem{}
	rawResponses := []*ModelResponse{}
	shouldRunStartHooks := true
	startTurn := 1

	// When resuming from an interruption, seed prior state and re-process the
	// captured response on the first iteration instead of calling the model.
	var pendingResponse *ModelResponse
	if r.resume != nil {
		currentAgent = r.resume.CurrentAgent
		originalInput = r.resume.OriginalInput
		generatedItems = append([]RunItem{}, r.resume.GeneratedItems...)
		rawResponses = append([]*ModelResponse{}, r.resume.RawResponses...)
		pendingResponse = r.resume.InterruptedResponse
		sessionSeed := r.resume.SessionItems
		if sessionSeed == nil {
			sessionSeed = r.resume.GeneratedItems
		}
		r.sessionItems = append([]RunItem{}, sessionSeed...)
		// The interrupted run already persisted its user input and every turn up
		// to the pause (holding back the pending, output-less tool calls), so the
		// resume continues from that cursor instead of re-saving.
		r.persistedSessionItems = r.resume.PersistedSessionItems
		r.userInputSaved = true
		// Continue counting turns where the interrupted run stopped, so repeated
		// interrupt/resume cycles cannot exceed the turn budget.
		if r.resume.CurrentTurn > 1 {
			startTurn = r.resume.CurrentTurn
		}
	}

	// previous_response_id tracking: previousResponseID chains server-side
	// conversation state; serverItemCount marks how many generated items the
	// server already has (so we only send the rest).
	var previousResponseID string
	var serverItemCount int
	var serverCursorActive bool

	// pending holds a snapshot supplied by PrepareNextTurn at the last save
	// point, to be used instead of resolving the next turn from the agent.
	var pending *TurnSnapshot

	// finalTurn marks the extra tool-free turn granted when the budget ran out,
	// so it is granted once rather than every turn from then on.
	var finalTurn bool

	// Persist the new user input up front (original run only; a resume's input
	// was saved before it paused). Mirrors Python persisting input before the
	// Runs defer the one-time user-input save to just before the first model
	// call (see below), so a failure ahead of that — a blocking input-guardrail
	// tripwire, a bad model config — leaves no orphan user message behind.

	r.log.Info(ctx, "run started",
		slog.Int("max_turns", r.maxTurns),
		slog.Int("tools", len(currentAgent.Tools)),
		slog.Bool("session", r.opts.Conversation.Session != nil))

	// Announce the starting agent before the first turn, for both fresh and
	// resumed runs.
	r.ctrl.setCurrent(currentAgent, startTurn)
	if !r.emit(&AgentUpdatedStreamEvent{NewAgent: currentAgent}) {
		return nil, errConsumerStopped
	}

	for turn := startTurn; ; turn++ {
		// After a completed turn, a caller may ask a streamed run to stop
		// gracefully: the current turn (incl. tools + session save) has finished,
		// so return cleanly with no error before starting the next one.
		if turn > startTurn && r.ctrl.stopRequested() {
			return &RunResult{
				Input:            originalInput,
				NewItems:         r.sessionItems,
				RawResponses:     rawResponses,
				LastAgent:        currentAgent,
				Usage:            r.rc.Usage,
				GuardrailResults: r.snapshotGuardrailResults(),
				Diagnostics:      r.diagnostics.All(),
			}, nil
		}
		if r.maxTurns > 0 && turn > r.maxTurns {
			// One last call with no tools lets the model close out in prose
			// instead of the run ending on an error. It spends a call the
			// budget said not to spend, so it is opt-in — and it is granted by
			// running THIS turn tool-free, not by skipping to the next one.
			// Once granted, the next overrun is the real end.
			if r.opts.Exec.ToolLoop.FinalTurnWithoutTools && !finalTurn {
				r.log.Info(ctx, "turn budget exhausted; one final turn without tools",
					slog.Int("max_turns", r.maxTurns))
				finalTurn = true
			} else {
				r.log.Warn(ctx, "turn budget exhausted", slog.Int("max_turns", r.maxTurns))
				maxErr := newMaxTurnsError(r.maxTurns)
				res, rerr := r.recoverMaxTurns(ctx, maxErr, originalInput, rawResponses, currentAgent)
				if rerr != nil {
					return nil, r.fail(rerr, originalInput, generatedItems, rawResponses, currentAgent)
				}
				if res != nil {
					return res, nil
				}
				return nil, r.fail(maxErr, originalInput, generatedItems, rawResponses, currentAgent)
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Publish the live turn/agent for RunControl (guarded by a mutex; the
		// run loop and the caller race).
		r.ctrl.setCurrent(currentAgent, turn)
		r.log.Debug(ctx, "turn started", slog.Int("turn", turn), slog.String("agent", currentAgent.Name))

		if shouldRunStartHooks {
			if currentAgent.OnStart != nil {
				if err := currentAgent.OnStart(ctx, r.rc); err != nil {
					return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
				}
			}
			shouldRunStartHooks = false
			// Start an agent span (parent of this agent's generation/tool spans).
			if r.agentSpan != nil {
				r.agentSpan.Finish()
			}
			r.agentSpan = r.trace.StartAgentSpan(currentAgent.Name, r.opts.parentSpanID)
		}

		// Build the model input. In previous_response_id mode, send only the
		// items the server does not yet have; otherwise send the full history.
		var turnInput []TResponseInputItem
		var prevID string
		var inputErr error
		switch {
		case r.opts.Conversation.UsePreviousResponseID && previousResponseID != "":
			turnInput, inputErr = itemsToInputList(generatedItems[serverItemCount:])
			prevID = previousResponseID
		case r.opts.Conversation.ConversationID != "" && serverCursorActive:
			// The conversation already holds prior items server-side.
			turnInput, inputErr = itemsToInputList(generatedItems[serverItemCount:])
		default:
			turnInput, inputErr = buildModelInput(originalInput, generatedItems)
		}
		if inputErr != nil {
			return nil, r.fail(inputErr, originalInput, generatedItems, rawResponses, currentAgent)
		}
		// Optionally strip reasoning-item ids before sending them to the model.
		turnInput = applyReasoningItemIDPolicy(turnInput, r.opts.Exec.ReasoningItemIDPolicy)

		snapshot, err := r.buildSnapshot(ctx, currentAgent, turnInput)
		if err != nil {
			return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
		}
		// A turn hook may have replaced the snapshot at the previous save
		// point; from here on the turn reads it and not the agent. Its Input is
		// overwritten: a prepared snapshot is almost always a copy of the
		// previous turn's, and honoring its Input would replay that turn — the
		// tool call and its output silently gone from what the model is sent.
		if pending != nil {
			pending.Input = turnInput
			snapshot = pending
			pending = nil
		}
		model, systemPrompt, prompt := snapshot.Model, snapshot.Instructions, snapshot.Prompt
		outputSchema, handoffs, tools := snapshot.OutputSchema, snapshot.Handoffs, snapshot.Tools
		modelInput := snapshot.Input
		if finalTurn {
			// The point of the extra turn is that the model CANNOT call
			// anything: offered a tool it would call one, and the budget would
			// be exhausted again with nothing said.
			tools, handoffs = nil, nil
		}

		// Publish the turn's input so input guardrails, hooks and tools all see
		// exactly what the model is being sent. CallModelInputFilter may still
		// edit it below, in which case this is refreshed.
		r.rc.setTurnInput(modelInput)

		// On the first turn, run input guardrails (run-level ones first, then the
		// starting agent's). A guardrail with Blocking=true runs to
		// completion BEFORE the model call — a gate. The rest run
		// concurrently with the model call for blocking runs (matching the Python
		// SDK), synchronously before it for streaming runs (the documented
		// difference); guardCh delivers their results and tripwire. They already
		// ran before an interruption, so a resumed run skips them.
		var guardCh chan inputGuardOutcome
		if turn == startTurn && r.resume == nil {
			all := selectStage(r.runGuardrails(startAgent), StageInput)
			var sequential, parallel []Guardrail
			for _, g := range all {
				if g.Blocking {
					sequential = append(sequential, g)
				} else {
					parallel = append(parallel, g)
				}
			}
			// Sequential (blocking) guardrails: a tripwire prevents the model call.
			if len(sequential) > 0 {
				gspan := r.trace.StartGuardrailSpan("input", r.agentParentID())
				res, gerr := runStageConcurrent(ctx, r.rc, sequential,
					GuardrailPayload{Stage: StageInput, Agent: startAgent, Input: originalInput})
				r.recordGuardrailResults(res...)
				if repl, ok := inputReplacement(res); ok {
					originalInput = repl
				}
				if gerr != nil {
					gspan.SetError(gerr.Error(), nil)
					gspan.Finish()
					return nil, r.fail(gerr, originalInput, generatedItems, rawResponses, currentAgent)
				}
				gspan.Finish()
			}
			// Non-blocking guardrails race the model call: a tripwire cancels it,
			// so the run fails without waiting for a response nobody will use.
			//
			// This used to be the blocking entry point's behavior only —
			// streaming ran them synchronously first, which is what the guardrail
			// docs called "the documented difference". There is one entry point
			// now, and racing is the behavior the docs describe.
			if len(parallel) > 0 {
				guardCh = make(chan inputGuardOutcome, 1)
				gctx, gcancel := context.WithCancel(ctx)
				cancelInputGuardrails = gcancel
				parentID := r.agentParentID() // read before the goroutine races a handoff
				go func() {
					gspan := r.trace.StartGuardrailSpan("input", parentID)
					res, gerr := runStageConcurrent(gctx, r.rc, parallel,
						GuardrailPayload{Stage: StageInput, Agent: startAgent, Input: originalInput})
					if gerr != nil {
						gspan.SetError(gerr.Error(), nil)
					}
					gspan.Finish()
					guardCh <- inputGuardOutcome{results: res, err: gerr}
				}()
			}
		}

		var resp *ModelResponse
		resumedTurn := pendingResponse != nil
		if resumedTurn {
			// Resuming: re-process the interrupted response (already counted in
			// usage and rawResponses) instead of calling the model again.
			resp = pendingResponse
			pendingResponse = nil
		} else {
			// The one-time user-input save. It lands here, not at loop start, so
			// a failure ahead of the first model call leaves no orphan user
			// message. What that covers depends on the guardrail:
			//
			//   - A Blocking input guardrail has already finished. A tripwire
			//     means nothing is persisted and the model is never called.
			//   - A racing one has not. Its tripwire arrives while the model
			//     call is in flight, so the input IS persisted and the model IS
			//     called (then cancelled) — the documented trade for not
			//     serializing every guardrail ahead of every model call.
			//
			// persistUserInput is idempotent via userInputSaved.
			if r.resume == nil {
				if err := r.persistUserInput(ctx); err != nil {
					return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
				}
			}
			if r.opts.Model.InputFilter != nil {
				edited, ferr := r.opts.Model.InputFilter(ctx, r.rc, currentAgent, ModelInputData{Instructions: systemPrompt, Input: modelInput})
				if ferr != nil {
					return nil, r.fail(ferr, originalInput, generatedItems, rawResponses, currentAgent)
				}
				systemPrompt, modelInput = edited.Instructions, edited.Input
				r.rc.setTurnInput(modelInput)
			}
			req := ModelRequest{
				SystemInstructions: systemPrompt,
				Prompt:             prompt,
				Input:              modelInput,
				Settings:           snapshot.Settings,
				Tools:              tools,
				OutputSchema:       outputSchema,
				Handoffs:           handoffs,
				Tracing:            ModelTracingDisabled,
				PreviousResponseID: prevID,
				ConversationID:     r.opts.Conversation.ConversationID,
			}
			r.ctrl.setPhase(PhaseModelCall)
			r.log.Debug(ctx, "calling model",
				slog.Int("turn", turn),
				slog.Int("input_items", len(modelInput)),
				slog.Int("tools", len(tools)),
				Sensitive("instructions", systemPrompt))
			span := r.startGenerationSpan(currentAgent, req)
			// Retries happen inside the model call, where the runner cannot
			// reach; the span rides on the context so they nest under it.
			callCtx := tracing.WithSpan(ctx, span)
			switch {
			case guardCh != nil:
				// Blocking run with first-turn parallel input guardrails: race the
				// model call against them so a tripwire cancels the in-flight call.
				// A tripped guardrail aborts the turn WITHOUT billing usage or
				// firing OnLLMEnd — the model task is discarded (Python parity:
				// should_cancel_parallel_model_task_on_input_guardrail_trip).
				modelCtx, modelCancel := context.WithCancel(callCtx)
				ch := make(chan modelCallOutcome, 1)
				go func() {
					rr, ee := model.GetResponse(modelCtx, req)
					ch <- modelCallOutcome{resp: rr, err: ee}
				}()
				var tripwire error
				readGuard := func(g inputGuardOutcome) {
					r.recordGuardrailResults(g.results...)
					tripwire = g.err
				}
				select {
				case g := <-guardCh:
					readGuard(g)
					if tripwire == nil {
						out := <-ch
						resp, err = out.resp, out.err
					}
				case out := <-ch:
					// Model finished first; honor a tripwire verdict still in flight.
					readGuard(<-guardCh)
					if tripwire == nil {
						resp, err = out.resp, out.err
					}
				}
				modelCancel()
				if tripwire != nil {
					span.SetError(tripwire.Error(), nil)
					span.Finish()
					return nil, r.fail(tripwire, originalInput, generatedItems, rawResponses, currentAgent)
				}
			case r.rawEvents:
				resp, err = r.streamOneModelCall(callCtx, span, model, req)
			default:
				resp, err = model.GetResponse(callCtx, req)
			}
			if err != nil {
				span.SetError(err.Error(), nil)
				span.Finish()
				return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
			}
			r.finishGenerationSpan(span, resp)
			// The model call completed and any first-turn input guardrails passed,
			// so bill usage and surface the response to OnLLMEnd.
			r.rc.Usage.Add(resp.Usage)
			rawResponses = append(rawResponses, resp)
		}
		r.log.Debug(ctx, "model responded",
			slog.Int("turn", turn),
			slog.String("response_id", resp.ResponseID),
			slog.Int("output_items", len(resp.Output)),
			slog.String("status", resp.Status),
			slog.Int64("input_tokens", usageOr(resp.Usage).InputTokens),
			slog.Int64("output_tokens", usageOr(resp.Usage).OutputTokens))
		r.lastResponseID = resp.ResponseID
		r.lastUsage = resp.Usage
		r.usagePending = true
		r.lastStore = r.resolveSettings(currentAgent).Store

		processed, err := processModelResponse(currentAgent, tools, handoffs, resp, r.opts.Exec.ToolNotFoundBehavior)
		if err != nil {
			return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
		}

		// Emit the model-produced items. A resumed turn re-processes the
		// interrupted response whose items the paused segment already emitted,
		// so only a fresh model call emits here.
		if !resumedTurn {
			for _, it := range processed.NewItems {
				if !r.emitItem(it) {
					return nil, errConsumerStopped
				}
			}
		}

		lenBeforeStep := len(generatedItems)
		step, err := r.executeToolsAndSideEffects(ctx, currentAgent, processed, outputSchema, resumedTurn, originalInput, generatedItems, resp)
		if err != nil {
			return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
		}

		// Emit items produced by side effects (tool/handoff outputs). On a fresh
		// turn NewStepItems begins with processed.NewItems (already emitted
		// above); on a resumed turn it holds only side-effect items, so
		// everything there is new to the stream.
		emitFrom := len(processed.NewItems)
		if resumedTurn {
			emitFrom = 0
		}
		for _, it := range step.NewStepItems[emitFrom:] {
			if !r.emitItem(it) {
				return nil, errConsumerStopped
			}
		}

		generatedItems = append(generatedItems, step.NewStepItems...)
		r.sessionItems = append(r.sessionItems, step.NewStepItems...)
		if len(processed.ToolsUsed) > 0 {
			r.markToolsUsed(currentAgent)
		}

		// Advance the server cursor: items the server already has are everything
		// sent this turn plus the model's own output items; synthesized items
		// (tool outputs) remain pending for the next call.
		if r.opts.Conversation.UsePreviousResponseID && resp.ResponseID != "" {
			previousResponseID = resp.ResponseID
			if resumedTurn {
				// The interrupted response's own items were already recorded
				// before the run paused; only this turn's tool outputs pend.
				serverItemCount = lenBeforeStep
			} else {
				serverItemCount = lenBeforeStep + len(processed.NewItems)
			}
		}
		// conversation_id mode: the server appends each turn's items, so advance
		// the cursor and send only deltas from the next turn on.
		if r.opts.Conversation.ConversationID != "" {
			serverCursorActive = true
			if resumedTurn {
				serverItemCount = lenBeforeStep
			} else {
				serverItemCount = lenBeforeStep + len(processed.NewItems)
			}
		}

		switch step.NextStep {
		case stepFinalOutput:
			// A steer that arrived too late for the save point, or a queued
			// follow-up, continues the run instead of ending it. The exchange
			// finished on its own terms; the next one starts from it, in the
			// same run, so the trace, the usage total and the session stay one
			// thing rather than three loosely related ones.
			if extra := r.ctrl.takeContinuation(); len(extra) > 0 {
				if err := r.persistSessionItems(ctx); err != nil {
					return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
				}
				injected := injectedInput(currentAgent, extra)
				generatedItems = append(generatedItems, injected...)
				r.sessionItems = append(r.sessionItems, injected...)
				for _, it := range injected {
					if !r.emitItem(it) {
						return nil, errConsumerStopped
					}
				}
				continue
			}
			res, ferr := r.finishRun(ctx, currentAgent, originalInput, rawResponses, step.FinalOutput)
			if ferr != nil {
				return nil, r.fail(ferr, originalInput, generatedItems, rawResponses, currentAgent)
			}
			return res, nil
		case stepHandoff:
			// Persist this turn before switching agents. sessionItems is the
			// unfiltered log, so the handoff input filter below (which rewrites
			// generatedItems) never affects what the session keeps.
			if err := r.persistSessionItems(ctx); err != nil {
				return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
			}
			// A handoff is a turn boundary too: control is about to leave this
			// agent, which is exactly the moment a caller may want to stop.
			// Asked before the input filter runs, so the hook sees the turn as
			// it happened. The rest of the save point does not apply — the next
			// turn belongs to a different agent, so its snapshot is resolved
			// fresh and its context is about to be rewritten by the filter.
			stop, out, serr := r.stopAfterTurn(ctx, currentAgent, turn, resp, snapshot, step.NewStepItems)
			if serr != nil {
				return nil, r.fail(serr, originalInput, generatedItems, rawResponses, currentAgent)
			}
			if stop {
				res, ferr := r.finishRun(ctx, currentAgent, originalInput, rawResponses, out)
				if ferr != nil {
					return nil, r.fail(ferr, originalInput, generatedItems, rawResponses, currentAgent)
				}
				return res, nil
			}
			if step.Handoff != nil {
				if filter := r.handoffInputFilter(step.Handoff); filter != nil {
					// A handoff input filter cannot coexist with server-managed
					// conversation state: the server holds the unfiltered history,
					// so a filtered view would desync (in ConversationID mode,
					// resending the full filtered input duplicates the server's
					// stored items). Fail fast, matching Python's UserError.
					if r.opts.Conversation.UsePreviousResponseID || r.opts.Conversation.ConversationID != "" {
						err := newUserError("handoff input filters (including NestHandoffHistory) are not supported with server-managed conversation state (UsePreviousResponseID / ConversationID)")
						return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
					}
					filtered, ferr := applyHandoffInputFilter(filter, originalInput, generatedItems)
					if ferr != nil {
						return nil, r.fail(ferr, originalInput, generatedItems, rawResponses, currentAgent)
					}
					originalInput = filtered
					generatedItems = nil
				}
			}
			r.log.Info(ctx, "handoff",
				slog.String("from", currentAgent.Name), slog.String("to", step.NewAgent.Name))
			currentAgent = step.NewAgent
			shouldRunStartHooks = true
			if !r.emit(&AgentUpdatedStreamEvent{NewAgent: currentAgent}) {
				return nil, errConsumerStopped
			}
			continue
		case stepInterruption:
			// Persist the completed part of this turn before pausing. The pending
			// tool calls have no outputs yet, so persistSessionItems holds them
			// back (they would break replay); they save with their outputs once
			// the run resumes. The cursor rides along in RunState.
			if err := r.persistSessionItems(ctx); err != nil {
				return nil, r.fail(err, originalInput, generatedItems, rawResponses, currentAgent)
			}
			// Snapshot any nested states already cached on the run context under the
			// mutex that guards them (run_context.go's nestedMu contract): a
			// timed-out tool can leave an orphan goroutine that still calls
			// takeNestedToolState concurrently with this read.
			r.rc.nestedMu.Lock()
			carriedNested := maps.Clone(r.rc.nestedToolStates)
			r.rc.nestedMu.Unlock()
			state := &RunState{
				CurrentAgent:          currentAgent,
				OriginalInput:         originalInput,
				GeneratedItems:        generatedItems,
				SessionItems:          r.sessionItems,
				PersistedSessionItems: r.persistedSessionItems,
				UserInput:             r.userInput,
				RawResponses:          rawResponses,
				InterruptedResponse:   resp,
				Interruptions:         step.Interruptions,
				Approvals:             r.rc.Approvals,
				Usage:                 r.rc.Usage,
				CurrentTurn:           turn,
				MaxTurns:              r.maxTurns,
				ToolsUsed:             toolsUsedList(r.toolsUsedBy),
				PendingInput:          r.ctrl.Pending(),
				ReasoningItemIDPolicy: r.opts.Exec.ReasoningItemIDPolicy,
				// Carry the guardrail results accumulated so far so a resumed run's
				// RunResult still reports them: first-turn input guardrails are not
				// re-run on resume (Python parity), so this is their only source.
				GuardrailResults: r.snapshotGuardrailResults(),
				// Carry any paused agent-as-tool nested states so ResumeRun
				// continues them; merge with any already cached on the run context
				// from an earlier resume of the same parent run. Serialized in
				// RunState JSON, so a cross-process resume continues them too.
				nestedToolStates: mergeNestedStates(carriedNested, step.NestedStates),
			}
			return &RunResult{
				Input: originalInput,
				// Unfiltered log for observability (State.GeneratedItems keeps the
				// filtered view for resume correctness).
				NewItems:         r.sessionItems,
				RawResponses:     rawResponses,
				LastAgent:        currentAgent,
				Usage:            r.rc.Usage,
				GuardrailResults: r.snapshotGuardrailResults(),
				Diagnostics:      r.diagnostics.All(),
				Interruptions:    step.Interruptions,
				State:            state,
			}, nil
		case stepRunAgain:
			sp, serr := r.savePoint(ctx, savePointInput{
				Turn:     turn,
				Agent:    currentAgent,
				Snapshot: snapshot,
				Response: resp,
				NewItems: step.NewStepItems,
			})
			if serr != nil {
				return nil, r.fail(serr, originalInput, generatedItems, rawResponses, currentAgent)
			}
			if sp.Stop {
				res, ferr := r.finishRun(ctx, currentAgent, originalInput, rawResponses, sp.FinalOutput)
				if ferr != nil {
					return nil, r.fail(ferr, originalInput, generatedItems, rawResponses, currentAgent)
				}
				return res, nil
			}
			if sp.Recompacted {
				// The rebuilt context already contains this run's items, so the
				// generated list starts over — the same substitution a handoff
				// input filter makes above.
				originalInput = sp.Input
				generatedItems = nil
			}
			if len(sp.Injected) > 0 {
				generatedItems = append(generatedItems, sp.Injected...)
				r.sessionItems = append(r.sessionItems, sp.Injected...)
				for _, it := range sp.Injected {
					if !r.emitItem(it) {
						return nil, errConsumerStopped
					}
				}
			}
			pending = sp.NextSnapshot
			continue
		}
	}
}

// finishRun is the final-output tail shared by the normal completion path and
// a max-turns recovery. Order: the agent-end hook fires FIRST (matching Python;
// before output guardrails — a tripped
// guardrail does not suppress on_agent_end), then output guardrails, then
// session persistence and compaction (kept after guardrails, matching Python's
// streaming path: a guardrail-tripped final output is not persisted).
func (r *runner) finishRun(ctx context.Context, agent *Agent, originalInput []TResponseInputItem, raw []*ModelResponse, finalOutput any) (*RunResult, error) {
	if agent.OnEnd != nil {
		if err := agent.OnEnd(ctx, r.rc, finalOutput); err != nil {
			return nil, err
		}
	}
	// Output guardrails: run-level ones first, then the producing agent's.
	// A Replace decision substitutes the final output and the run continues.
	if outGuardrails := selectStage(r.runGuardrails(agent), StageOutput); len(outGuardrails) > 0 {
		gspan := r.trace.StartGuardrailSpan("output", r.agentParentID())
		res, gerr := runStageConcurrent(ctx, r.rc, outGuardrails,
			GuardrailPayload{Stage: StageOutput, Agent: agent, Output: finalOutput})
		r.recordGuardrailResults(res...)
		if gerr != nil {
			gspan.SetError(gerr.Error(), nil)
			gspan.Finish()
			return nil, gerr
		}
		gspan.Finish()
		for _, g := range res {
			if g.Decision.Action == GuardrailReplace {
				finalOutput = g.Decision.Message
				break
			}
		}
	}
	if err := r.persistSessionItems(ctx); err != nil {
		return nil, err
	}
	r.compactAfterRun(ctx)
	return &RunResult{
		Input: originalInput,
		// The unfiltered item log: a handoff input filter rewrites the model's
		// view (generatedItems) but never what the result reports (Python parity:
		// new_items = session_items).
		NewItems:         r.sessionItems,
		RawResponses:     raw,
		FinalOutput:      finalOutput,
		LastAgent:        agent,
		Usage:            r.rc.Usage,
		GuardrailResults: r.snapshotGuardrailResults(),
		Diagnostics:      r.diagnostics.All(),
	}, nil
}

// recoverMaxTurns gives ErrorHandlers.MaxTurns a chance to turn a turn-budget
// overrun into a normal completion. It returns (nil, nil) when there is no
// handler or it declines — the caller then fails with the MaxTurnsError. On
// recovery the agent span still records the overrun (Python parity: the error
// is traced even when handled), the synthesized fallback message joins the
// run's items and session unless the handler opted out, and the run finishes
// through the same guardrail/persist/hook tail as a normal final output.
func (r *runner) recoverMaxTurns(ctx context.Context, cause *MaxTurnsError, originalInput []TResponseInputItem, rawResponses []*ModelResponse, agent *Agent) (*RunResult, error) {
	// Handlers see the session view of the run (never reset by handoff input
	// filters), like Python's session_items-based RunErrorData for max_turns.
	rec, err := r.resolveErrorRecovery(ctx, "max_turns", r.opts.Exec.ErrorHandlers.MaxTurns, cause, agent, originalInput, r.sessionItems, rawResponses)
	if err != nil || rec == nil {
		return nil, err
	}
	r.agentSpan.SetError(cause.Error(), map[string]any{"max_turns": r.maxTurns})
	if rec.message != nil {
		// finishRun reports r.sessionItems as NewItems, so the synthesized
		// fallback message joins the run there (and the session).
		r.sessionItems = append(r.sessionItems, rec.message)
		if !r.emit(&RunItemStreamEvent{Name: runItemEventName(rec.message), Item: rec.message}) {
			return nil, errConsumerStopped
		}
	}
	return r.finishRun(ctx, agent, originalInput, rawResponses, rec.finalOutput)
}

func (r *runner) fail(err error, input []TResponseInputItem, items []RunItem, raw []*ModelResponse, last *Agent) error {
	// Mark the current agent span failed so the error is visible in traces;
	// child spans (generation, function) set their own errors at the source.
	r.agentSpan.SetError(err.Error(), nil)
	// Report the unfiltered item log when a handoff input filter has reset the
	// caller's generatedItems view (Python parity: RunErrorDetails carries
	// session_items). Without a filter the two are identical.
	newItems := items
	if len(r.sessionItems) > len(items) {
		newItems = r.sessionItems
	}
	details := &RunErrorDetails{
		Input:            input,
		NewItems:         newItems,
		RawResponses:     raw,
		LastAgent:        last,
		Usage:            r.rc.Usage,
		GuardrailResults: r.snapshotGuardrailResults(),
		Diagnostics:      r.diagnostics.All(),
	}
	var ae *AgentsError
	if asAgentsError(err, &ae) {
		ae.Details = details
		return err
	}
	return err
}

// persistUserInput writes the run's new user input to the session once, at loop
// start. Later per-turn saves persist only generated items, so the prompt is
// never rewritten. No-op without a session or when there is no new input.
func (r *runner) persistUserInput(ctx context.Context) error {
	if r.opts.Conversation.Session == nil || r.userInputSaved || len(r.userInput) == 0 {
		return nil
	}
	if err := r.opts.Conversation.Session.AppendItems(ctx, r.userInput, Source{Type: SourceUser}); err != nil {
		return err
	}
	r.userInputSaved = true
	return nil
}

// persistSessionItems incrementally saves the sessionItems produced since the
// last save. It persists only the "safe" leading prefix: a trailing
// function_call still awaiting its output (a HITL pause) is held back so the
// stored conversation never contains a call without its output, which would be
// rejected on replay. The held-back calls save on the next turn, once their
// outputs arrive. sessionItems is the unfiltered log, so handoff input filters
// never affect what is stored.
func (r *runner) persistSessionItems(ctx context.Context) error {
	if r.opts.Conversation.Session == nil {
		return nil
	}
	end := safePersistBoundary(r.sessionItems, r.persistedSessionItems)
	if end <= r.persistedSessionItems {
		return nil
	}
	toSave := make([]SessionEntry, 0, end-r.persistedSessionItems)
	for _, it := range r.sessionItems[r.persistedSessionItems:end] {
		// Provenance and display ride along, so a reader gets the same timeline
		// the run produced instead of re-deriving it from the wire item.
		e, err := EntryFromRunItem(it, r.lastResponseID)
		if err != nil {
			return err
		}
		toSave = append(toSave, e)
	}
	r.attributeUsage(toSave)
	r.attributeDiagnostics(toSave)
	if len(toSave) > 0 {
		if err := r.opts.Conversation.Session.Append(ctx, toSave...); err != nil {
			return err
		}
		r.log.Debug(ctx, "turn persisted", slog.Int("entries", len(toSave)))
	}
	r.persistedSessionItems = end
	return nil
}

// safePersistBoundary returns the exclusive end index up to which items[start:]
// can be safely persisted without ever storing a function_call that lacks its
// matching function_call_output. It returns the largest end such that every
// function_call in items[start:end] has its output also within items[start:end).
//
// Scanning left to right, the boundary advances to just past each point where no
// call is left open (awaiting its output). A pending call — and everything
// ordered after it, including a completed sibling's output that happens to sit
// after it (as at a nested agent-as-tool pause: [call S, call A(pending),
// output S]) — is held back until the missing outputs arrive on resume, so the
// stored history never contains a dangling call. A turn whose calls are all
// paired therefore persists in full.
func safePersistBoundary(items []RunItem, start int) int {
	if start >= len(items) {
		return len(items)
	}
	end := start
	open := map[string]struct{}{}
	for i := start; i < len(items); i++ {
		id, isCall, isOutput := runItemCallID(items[i])
		switch {
		case isCall:
			open[id] = struct{}{}
		case isOutput:
			delete(open, id)
		}
		if len(open) == 0 {
			end = i + 1
		}
	}
	return end
}

// runItemCallID reports a run item's function-call correlation id and whether it
// is a call or an output, by inspecting its input-item form. Non-function items
// (messages, reasoning, handoffs) report isCall=isOutput=false. Works uniformly
// for live items and items rebuilt from serialized RunState.
func runItemCallID(it RunItem) (callID string, isCall, isOutput bool) {
	in, err := it.ToInputItem()
	if err != nil {
		return "", false, false
	}
	switch {
	case in.OfFunctionCall != nil:
		return in.OfFunctionCall.CallID, true, false
	case in.OfFunctionCallOutput != nil:
		return in.OfFunctionCallOutput.CallID, false, true
	}
	return "", false, false
}

// compactAfterRun is the CompactAfterRun point: the run's items are persisted
// and its final output produced, so a self-compacting storage gets its turn to
// shrink what it keeps.
//
// It is best-effort housekeeping: a failure is recorded on the trace instead of
// turning a successful run into a failed one.
func (r *runner) compactAfterRun(ctx context.Context) {
	if r.opts.Conversation.Session == nil {
		return
	}
	// A configured Compactor records its result as a checkpoint. It and a
	// self-compacting storage never both apply: compactContext stands aside
	// when the storage compacts itself.
	if r.checkpointAfterRun(ctx) {
		return
	}
	// Items produced locally AFTER the last model response — a final turn's
	// tool/handoff outputs (a terminating tool, rejected calls) or a synthesized
	// error-handler fallback message — are not on the server's
	// previous_response_id chain, so compacting from lastResponseID would
	// erase them from the stored history. Python defers compaction for such
	// turns (save_result_to_session's has_local_tool_outputs check); with one
	// compaction per run, ending on such a turn means skipping it.
	if endsWithLocalItem(r.sessionItems) {
		return
	}
	if cs, ok := r.opts.Conversation.Session.Storage().(CompactionAware); ok {
		// The span starts lazily — only when the session actually compacts —
		// so no-op passes don't clutter the trace.
		var cspan *tracing.SpanHandle
		cerr := cs.RunCompaction(ctx, CompactionArgs{
			ResponseID: r.lastResponseID,
			Store:      r.lastStore,
			StartSpan: func() *tracing.SpanHandle {
				cspan = r.trace.StartCompactionSpan(r.agentParentID())
				return cspan
			},
		})
		if cerr != nil && cspan == nil {
			// Failed before the session opened the span; open one so the
			// error is still visible on the trace.
			cspan = r.trace.StartCompactionSpan(r.agentParentID())
		}
		if cspan != nil {
			if cerr != nil {
				cspan.SetError(cerr.Error(), nil)
			}
			cspan.Finish()
		}
	}
}

// endsWithLocalItem reports whether the run's last item was produced locally
// by the SDK rather than returned by the model: a tool/handoff output, or an
// error-handler's synthesized fallback message (marked with the fake response
// id). Such items postdate the last model response and are absent from the
// server-side response chain that previous_response_id compaction replays.
func endsWithLocalItem(items []RunItem) bool {
	if len(items) == 0 {
		return false
	}
	// Anything the runner synthesized — a tool output, a handoff
	// acknowledgement, an error handler's fallback message — is local. The
	// model's own output and the caller's input are not.
	//
	// This used to be a type switch that string-compared a sentinel id on
	// messages and re-derived the answer from a kind string on restored items;
	// provenance answers it directly, and correctly for item types added later.
	return !items[len(items)-1].Source().IsExternal()
}

// mergeNestedStates combines any agent-as-tool nested states still cached on
// the run context (un-consumed from a prior resume) with those freshly paused
// this turn, preferring the fresh ones. Returns nil when both are empty so a
// run without nested-tool HITL carries no map.
func mergeNestedStates(carried, fresh map[string]*RunState) map[string]*RunState {
	if len(carried) == 0 && len(fresh) == 0 {
		return nil
	}
	out := make(map[string]*RunState, len(carried)+len(fresh))
	maps.Copy(out, carried)
	maps.Copy(out, fresh)
	return out
}

// validateServerState rejects incompatible server-managed conversation options.
// conversation_id and previous_response_id both put history on the server, so
// they cannot be combined with each other or with a local Session.
func validateServerState(opts RunOptions) error {
	if opts.Conversation.ConversationID != "" {
		if opts.Conversation.UsePreviousResponseID {
			return newUserError("ConversationID cannot be combined with UsePreviousResponseID")
		}
		if opts.Conversation.Session != nil {
			return newUserError("ConversationID cannot be combined with a local Session")
		}
	}
	if opts.Conversation.UsePreviousResponseID && opts.Conversation.Session != nil {
		return newUserError("UsePreviousResponseID cannot be combined with a local Session")
	}
	return nil
}

// toolsUsedList returns the agent names in a tool-use tracker as a slice, for
// carrying the tool_choice reset across an interrupt/resume in RunState.
func toolsUsedList(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	return slices.Sorted(maps.Keys(m))
}

// markToolsUsed records that agent called tools this run (for tool_choice reset).
func (r *runner) markToolsUsed(agent *Agent) {
	if r.toolsUsedBy == nil {
		r.toolsUsedBy = map[string]bool{}
	}
	r.toolsUsedBy[agent.Name] = true
}

// resolveModel returns the Model for the given agent, honoring (in order) the
// agent's explicit ModelImpl, the run-level Model override, then the provider.
// setGenerationUsage records a single model call's token counts on its
// generation span, so trace consumers see per-call input/output/total tokens
// (rc.Usage holds the run-wide accumulation separately).
func setGenerationUsage(span *tracing.SpanHandle, u *Usage) {
	if u == nil {
		return
	}
	span.Set("input_tokens", u.InputTokens)
	span.Set("output_tokens", u.OutputTokens)
	span.Set("total_tokens", u.TotalTokens)
}

// traceIncludeSensitiveData resolves RunOptions.Observe.IncludeSensitiveData,
// falling back to the OPENAI_AGENTS_TRACE_INCLUDE_SENSITIVE_DATA environment
// variable where anything but "false" means true — the same default as the
// Python SDK.
func (r *runner) traceIncludeSensitiveData() bool {
	if r.opts.Observe.IncludeSensitiveData != nil {
		return *r.opts.Observe.IncludeSensitiveData
	}
	return !strings.EqualFold(strings.TrimSpace(os.Getenv("OPENAI_AGENTS_TRACE_INCLUDE_SENSITIVE_DATA")), "false")
}

// traceTools projects the request's tools into a serializable form — the
// function fields on FunctionTool cannot be marshaled.
func traceTools(tools []Tool) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		m := map[string]any{"name": t.ToolName()}
		if d, ok := ToolAs[DescribableTool](t); ok {
			if desc := d.ToolDescription(); desc != "" {
				m["description"] = desc
			}
			if schema := d.ToolParamsSchema(); schema != nil {
				m["parameters"] = schema
			}
		}
		out = append(out, m)
	}
	return out
}

// traceHandoffs projects the request's handoffs into a serializable form.
func traceHandoffs(handoffs []Handoff) []map[string]any {
	out := make([]map[string]any, 0, len(handoffs))
	for _, h := range handoffs {
		out = append(out, map[string]any{
			"tool_name":  h.ToolName,
			"agent_name": h.AgentName,
		})
	}
	return out
}

// startGenerationSpan opens the span for one model call and, unless
// sensitive-data tracing is off, records the full request body: model name,
// system instructions, input items, tool definitions, model settings (the
// Extra* passthrough fields are excluded by their json tags), handoffs, and
// output schema. Slices are cloned or projected because exporters serialize
// asynchronously, after the runner may have modified the originals.
func (r *runner) startGenerationSpan(agent *Agent, req ModelRequest) *tracing.SpanHandle {
	span := r.trace.StartGenerationSpan(agent.Name, r.agentParentID())
	if span.Span == nil || !r.traceIncludeSensitiveData() {
		return span
	}
	if agent.Model != "" {
		span.Set("model", agent.Model)
	}
	if req.SystemInstructions != "" {
		span.Set("system_instructions", req.SystemInstructions)
	}
	span.Set("input", slices.Clone(req.Input))
	if len(req.Tools) > 0 {
		span.Set("tools", traceTools(req.Tools))
	}
	if len(req.Handoffs) > 0 {
		span.Set("handoffs", traceHandoffs(req.Handoffs))
	}
	if req.Settings != nil {
		span.Set("model_settings", *req.Settings)
	}
	if req.OutputSchema != nil && !req.OutputSchema.IsPlainText() {
		span.Set("output_schema", map[string]any{
			"name":   req.OutputSchema.Name(),
			"schema": req.OutputSchema.JSONSchema(),
		})
	}
	if req.Prompt != nil {
		span.Set("prompt", *req.Prompt)
	}
	if req.PreviousResponseID != "" {
		span.Set("previous_response_id", req.PreviousResponseID)
	}
	if req.ConversationID != "" {
		span.Set("conversation_id", req.ConversationID)
	}
	return span
}

// finishGenerationSpan records the model call's outcome — response id, usage,
// and (unless sensitive-data tracing is off) the output items — and ends the
// span.
func (r *runner) finishGenerationSpan(span *tracing.SpanHandle, resp *ModelResponse) {
	span.Set("response_id", resp.ResponseID)
	setGenerationUsage(span, resp.Usage)
	if span.Span != nil && r.traceIncludeSensitiveData() {
		span.Set("output", slices.Clone(resp.Output))
	}
	span.Finish()
}

func (r *runner) resolveModel(agent *Agent) (Model, error) {
	if agent.ModelImpl != nil {
		return agent.ModelImpl, nil
	}
	if r.opts.Model.Override != nil {
		return r.opts.Model.Override, nil
	}
	if r.opts.Model.Provider != nil {
		return r.opts.Model.Provider.GetModel(agent.Model)
	}
	return nil, newUserError("no model available: set Agent.ModelImpl, RunOptions.Model.Override, or RunOptions.Model.Provider")
}

// resolveSettings merges the run-level settings override over the agent's own.
func (r *runner) resolveSettings(agent *Agent) *ModelSettings {
	base := agent.ModelSettings
	if base == nil {
		base = &ModelSettings{}
	}
	s := base.Resolve(r.opts.Model.Settings)
	// Once an agent has called tools, leave tool_choice unset on its later
	// turns so a "required"/specific-tool setting cannot force an infinite
	// tool-call loop (the Python SDK's reset_tool_choice behavior).
	if !agent.DisableToolChoiceReset && s.ToolChoice != "" && r.toolsUsedBy[agent.Name] {
		s.ToolChoice = ""
	}
	return s
}

// enabledTools returns the agent's tools, filtered by any IsEnabled predicate
// and augmented with the tools exposed by the agent's MCP servers.
func (r *runner) enabledTools(ctx context.Context, agent *Agent) ([]Tool, error) {
	out := make([]Tool, 0, len(agent.Tools))
	for _, t := range agent.Tools {
		// A tool built from an unusable schema/argument type is never sent to
		// the model — fail the run now with a *UserError instead of letting the
		// model call it and receive a schema error. Construction errors are
		// deferred so constructors stay single-valued, so the runner surfaces
		// them here.
		if ft, ok := ToolAs[*FunctionTool](t); ok && ft.constructionErr != nil {
			return nil, newUserError("tool %q: %v", ft.Name, ft.constructionErr)
		}
		if e, ok := ToolAs[EnableableTool](t); ok {
			enabled, err := e.IsToolEnabled(ctx, r.rc, agent)
			if err != nil {
				return nil, err
			}
			if !enabled {
				continue
			}
		}
		out = append(out, t)
	}
	for _, server := range agent.MCPServers {
		mcpTools, err := server.ListTools(ctx, r.rc, agent)
		if err != nil {
			slog.WarnContext(ctx, "MCP ListTools failed, skipping server",
				"agent", agent.Name, "error", err)
			continue
		}
		out = append(out, mcpTools...)
	}
	// Reject duplicate tool names instead of silently letting the last one
	// shadow the others in the runner's dispatch map (the Python SDK raises a
	// UserError for duplicates too).
	seen := make(map[string]bool, len(out))
	for _, t := range out {
		name := t.ToolName()
		if name == "" {
			continue
		}
		if seen[name] {
			return nil, newUserError("duplicate tool name %q on agent %q: tool names must be unique across Agent.Tools and MCP server tools", name, agent.Name)
		}
		seen[name] = true
	}
	return out, nil
}

// enabledHandoffs returns the agent's handoffs, filtered by any IsEnabled
// predicate (nil means enabled; a predicate error aborts the run). A disabled
// handoff is not offered to the model and cannot be invoked.
func (r *runner) enabledHandoffs(ctx context.Context, agent *Agent) ([]Handoff, error) {
	out := make([]Handoff, 0, len(agent.Handoffs))
	for _, h := range agent.Handoffs {
		if h.IsEnabled != nil {
			ok, err := h.IsEnabled(ctx, r.rc, agent)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
		}
		out = append(out, h)
	}
	return out, nil
}

func agentOutputSchema(agent *Agent) OutputSchema {
	if agent.OutputType != nil {
		return agent.OutputType
	}
	return PlainTextOutput()
}

// buildModelInput assembles the model input for a turn: the original input
// followed by every generated item converted back to input form.
func buildModelInput(originalInput []TResponseInputItem, generated []RunItem) ([]TResponseInputItem, error) {
	genInput, err := itemsToInputList(generated)
	if err != nil {
		return nil, err
	}
	out := make([]TResponseInputItem, 0, len(originalInput)+len(genInput))
	out = append(out, originalInput...)
	out = append(out, genInput...)
	return out, nil
}

// normalizeInput coerces a string or []TResponseInputItem into the input list.
func normalizeInput(input any) ([]TResponseInputItem, error) {
	switch v := input.(type) {
	case string:
		return InputItemsFromText(v), nil
	case []TResponseInputItem:
		return v, nil
	case nil:
		return nil, newUserError("run input must not be nil")
	default:
		return nil, newUserError("unsupported run input type %T (want string or []TResponseInputItem)", input)
	}
}

// asAgentsError finds the embedded AgentsError of any SDK error type in err's
// chain (unwrapping fmt.Errorf %w wrapping), so RunErrorDetails can be attached.
func asAgentsError(err error, target **AgentsError) bool {
	if c, ok := errors.AsType[agentsErrorCarrier](err); ok {
		*target = c.base()
		return true
	}
	return false
}
