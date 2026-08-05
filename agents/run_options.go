package agents

import (
	"context"

	"github.com/zzir/agents-go/tracing"
)

// DefaultMaxTurns is the turn budget applied when RunOptions.Exec.MaxTurns is zero.
const DefaultMaxTurns = 10

// MaxTurnsUnlimited disables the turn budget when set as
// RunOptions.Exec.MaxTurns — the run loops until it produces a final output,
// hands off to a finishing agent, or is cancelled. Use with care: a model that
// never finishes will loop indefinitely.
const MaxTurnsUnlimited = -1

// ModelInputData is the editable portion of a model call passed to a
// CallModelInputFilter: the system instructions and the input items.
type ModelInputData struct {
	Instructions string
	Input        []InputItem
}

// CallModelInputFilter edits the instructions and input items just before a
// model call. Returning an error aborts the run.
type CallModelInputFilter func(ctx context.Context, rc *RunContext, agent *Agent, data ModelInputData) (ModelInputData, error)

// RunOptions configures a run. The zero value is usable — agents.RunSync(ctx,
// agent, "hi", agents.RunOptions{}) works as long as the agent can resolve a
// model (an explicit Agent.ModelImpl, or Model.Override / Model.Provider here).
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
	// hooks via RunContext.Context. It is the single way user data enters a
	// run; the run wraps it in a fresh RunContext of its own.
	Context any

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

	// SessionSettings overrides how the run reads the Session (e.g. how many
	// recent items to load). Non-zero fields take precedence over a Session-level
	// default. Ignored without a Session.
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
	// rejection returns the guardrail message as the tool output without emitting
	// an approval request or executing the tool. Calls that pass still re-run the
	// same guardrails immediately before execution after approval, so
	// time-sensitive checks are revalidated on resume. Off by default.
	PreApprovalToolInputGuardrails bool

	// HandoffInputFilter is a run-level default applied to any handoff that does
	// not set its own Handoff.InputFilter. Use NestHandoffHistory to fold prior
	// history across all handoffs.
	HandoffInputFilter func(HandoffInputData) HandoffInputData

	// ErrorHandlers supplies per-error-kind recovery handlers that can turn a
	// failing run — max turns exceeded, a model refusal, or an invalid
	// structured final output — into a normal completion with a fallback final
	// output. The zero value leaves every error fatal.
	ErrorHandlers RunErrorHandlers

	// ToolLoop bounds the tool loop: consecutive all-failed turns, and what
	// happens when the turn budget runs out. The zero value is sensible.
	ToolLoop ToolLoopPolicy

	// Overflow decides what happens when a model call fails because the
	// context did not fit. The zero value returns the error unchanged.
	Overflow OverflowPolicy

	// ReasoningItemIDPolicy controls whether reasoning-item ids are kept when run
	// items are converted back into model input on later turns. The default
	// (ReasoningItemIDPreserve) keeps them; ReasoningItemIDOmit strips them. It
	// is persisted across interruptions in RunState.
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

	// TraceIncludeSensitiveData controls whether generation spans record the full
	// model request (model, system instructions, input items) and output items.
	// nil reads the OPENAI_AGENTS_TRACE_INCLUDE_SENSITIVE_DATA environment
	// variable, where anything but "false" means true. Set to false when trace
	// exports must not carry conversation content.
	IncludeSensitiveData *bool

	// TraceGroupID links this run's trace to a group of related traces (e.g. one
	// chat thread across several runs). Only used when Tracer starts a new trace.
	TraceGroupID string

	// TraceMetadata attaches user metadata to the run's trace. Only used when
	// Tracer starts a new trace.
	TraceMetadata map[string]any
}
