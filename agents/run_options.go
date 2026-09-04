package agents

import (
	"context"

	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/tracing"
)

// DefaultMaxTurns is the turn budget applied when RunOptions.Exec.MaxTurns is zero.
const DefaultMaxTurns = 10

// MaxTurnsUnlimited disables the turn budget when set as
// RunOptions.Exec.MaxTurns: the run loops until a final output, a finishing
// handoff or cancellation. A model that never finishes loops forever.
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

// RunOptions configures a run. The zero value works as long as the agent can
// resolve a model (Agent.ModelImpl, or Model.Override / Model.Provider here).
// Fields are grouped by what they configure; Conversation in particular
// collects options that constrain each other (spec §2.0b).
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

	// Middlewares wrap the run, outermost first. They are where optional policy
	// lives — logging, retrying, recovering — rather than a loop field each.
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

	// parentTrace, when set, records the run's spans into an existing trace
	// instead of starting its own; set internally for nested runs.
	parentTrace *tracing.TraceHandle

	// parentSpanID, with parentTrace, parents the nested run's agent spans
	// under the agent-as-tool call's function span. Set internally.
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
	Session *session.Session

	// Settings tunes how the run reads the Session — how many recent entries
	// to load. The zero value reads the whole history. Ignored without a
	// Session.
	Settings session.Settings

	// UsePreviousResponseID opts into server-managed conversation state: calls
	// chain via previous_response_id and only new items are sent. It needs a
	// model that returns response ids and keeps responses stored (do not set
	// ModelSettings.Store=false).
	UsePreviousResponseID bool

	// ConversationID attaches the run to a server-side OpenAI conversation
	// (the Responses API `conversation` parameter). Like UsePreviousResponseID,
	// the server holds history, so the runner sends only new items each turn. It
	// is server-managed state and must not be combined with a local Session.
	ConversationID string

	// Projectors overrides how session entries become model input, per entry
	// kind — the single answer to "what does the model get to read". The
	// defaults send items and compaction checkpoints and nothing else. A
	// projector mapped to nil suppresses that kind; the common override is the
	// opposite, projecting session.EntryKindTerminal as a user message.
	Projectors map[session.EntryKind]session.Projector
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

	// PreApprovalToolInputGuardrails runs a tool's input guardrails before
	// surfacing its approval interruption: a rejection returns the guardrail
	// message as the tool output without asking a human. Calls that pass re-run
	// them after approval too. Off by default.
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

	// ReasoningItemIDPolicy controls whether reasoning-item ids are kept when
	// run items go back to the model on later turns (default: kept). Persisted
	// across interruptions in RunState.
	ReasoningItemIDPolicy ReasoningItemIDPolicy

	// PrepareNextTurn rebuilds the next turn's configuration at the turn
	// boundary (nil leaves it to the usual resolution) — how a run changes
	// shape mid-flight, swapping a model or withdrawing a tool, without
	// mutating the Agent a concurrent run may be reading (spec §2.3b).
	PrepareNextTurn func(ctx context.Context, tr *TurnResult) (*TurnSnapshot, error)

	// ShouldStopAfterTurn ends the run after a turn that would otherwise
	// continue. Consulted at the save point, so a stopped run has its full
	// history saved. A predicate, not a producer: the final output is the
	// turn's last message text, else its last tool output (spec §2.3c).
	ShouldStopAfterTurn func(ctx context.Context, tr *TurnResult) (bool, error)
}

// ObserveOptions configures tracing for a run.
type ObserveOptions struct {

	// Tracer, when set, records a trace of the run with a span per model call.
	// Build one with tracing.NewTracer(processor).
	Tracer *tracing.Tracer

	// IncludeSensitiveData controls whether generation spans record the model
	// request and output items. nil means include. The SDK reads no
	// environment variable (spec §2.14).
	IncludeSensitiveData *bool

	// TraceGroupID links this run's trace to a group of related traces (e.g. one
	// chat thread across several runs). Only used when Tracer starts a new trace.
	TraceGroupID string

	// TraceMetadata attaches user metadata to the run's trace. Only used when
	// Tracer starts a new trace.
	TraceMetadata map[string]any
}
