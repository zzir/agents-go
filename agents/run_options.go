package agents

import (
	"context"

	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/tracing"
)

// DefaultMaxTurns is the turn budget applied when RunOptions.Exec.MaxTurns is zero.
const DefaultMaxTurns = 10

// DefaultToolOutputLimit is the cap, in bytes, on the text a tool result sends
// to the model when RunOptions.Exec.ToolOutputLimit is zero.
const DefaultToolOutputLimit = 64 << 10

// MaxTurnsUnlimited disables the turn budget when set as RunOptions.Exec.MaxTurns;
// a model that never finishes loops forever.
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

// RunOptions configures a run. The zero value works when the agent resolves a
// model itself (Agent.ModelImpl); fields are grouped by what they configure —
// see spec §2.0b.
type RunOptions struct {
	// Model selects and configures the model behind every agent in the run.
	Model ModelOptions

	// Conversation decides where history lives and how it joins the run's new input.
	Conversation ConversationOptions

	// Exec bounds and steers the loop itself.
	Exec ExecOptions

	// Compaction shrinks the model's context as the conversation grows; zero
	// disables it.
	Compaction CompactionOptions

	// Guardrails apply to the whole run, consulted before each agent's own at
	// every stage.
	Guardrails []Guardrail

	// Middlewares wrap the run, outermost first — see spec §2.12.
	Middlewares []RunMiddleware

	// Observe configures tracing.
	Observe ObserveOptions

	// Log configures the SDK's own structured logging; the zero value is silent.
	Log LogConfig

	// Context is arbitrary user data reachable from tools, guardrails and hooks
	// via RunContext.Context; the run wraps it in a fresh RunContext.
	Context any

	// parentTrace records the run's spans into an existing trace (nested runs).
	parentTrace *tracing.TraceHandle

	// parentSpanID parents a nested run's agent spans under the calling function span.
	parentSpanID string
}

// ModelOptions selects and configures the model used by every agent in a run.
type ModelOptions struct {

	// Provider resolves an agent's model name to a Model. Required unless every
	// agent sets ModelImpl or Override is set.
	Provider ModelProvider

	// Override replaces the model for every agent in the run; it beats Provider
	// lookups.
	Override Model

	// Settings is a run-level override merged over each agent's own ModelSettings.
	Settings *ModelSettings

	// InputFilter edits the instructions and input items just before each
	// model call. It does not change what is saved to the session.
	InputFilter CallModelInputFilter
}

// ConversationOptions decides where a run's history lives: in a local Session,
// or on the server via previous_response_id or a conversation id. A run that
// sets both is rejected.
type ConversationOptions struct {

	// Session supplies prior history as input and persists the run's new items.
	Session *session.Session

	// Settings tunes how the run reads the Session (how many recent entries to
	// load); the zero value reads the whole history.
	Settings session.Settings

	// UsePreviousResponseID chains calls via previous_response_id and sends
	// only new items. Needs response ids and stored responses (not
	// ModelSettings.Store=false).
	UsePreviousResponseID bool

	// ConversationID attaches the run to a server-side OpenAI conversation (the
	// Responses API `conversation` parameter); only new items are sent each turn.
	ConversationID string

	// Projectors overrides how each entry kind becomes model input. The defaults
	// send items and compaction checkpoints; nil suppresses a kind.
	Projectors map[session.EntryKind]session.Projector
}

// ExecOptions bounds and steers the run loop.
type ExecOptions struct {
	// MaxTurns bounds the model calls before the run fails with MaxTurnsError;
	// zero means DefaultMaxTurns.
	MaxTurns int

	// MaxToolConcurrency bounds how many function tools run concurrently within
	// one turn; zero means no limit.
	MaxToolConcurrency int

	// ToolNotFoundBehavior decides what a call to an unknown tool does: abort
	// the run (default) or feed an error back to the model.
	ToolNotFoundBehavior ToolNotFoundBehavior

	// PreApprovalToolInputGuardrails runs a tool's input guardrails before its
	// approval interruption; a rejection answers without asking. Off by default.
	PreApprovalToolInputGuardrails bool

	// HandoffInputFilter is the default for a handoff without its own
	// Handoff.InputFilter (NestHandoffHistory folds prior history).
	HandoffInputFilter func(HandoffInputData) HandoffInputData

	// ErrorHandlers turns max-turns, refusal or invalid-output failures into a
	// completion with a fallback output; the zero value leaves them fatal.
	ErrorHandlers RunErrorHandlers

	// ToolLoop bounds the tool loop: consecutive all-failed turns and what
	// happens when the turn budget runs out.
	ToolLoop ToolLoopPolicy

	// Overflow decides what a context-overflow model error does; the zero
	// value returns the error unchanged.
	Overflow OverflowPolicy

	// ReasoningItemIDPolicy decides whether reasoning-item ids are kept when run
	// items go back to the model (default: kept). Persisted in RunState.
	ReasoningItemIDPolicy ReasoningItemIDPolicy

	// PrepareNextTurn rebuilds the next turn's configuration at the turn
	// boundary; nil leaves it to the usual resolution — see spec §2.3b.
	PrepareNextTurn func(ctx context.Context, tr *TurnResult) (*TurnSnapshot, error)

	// ShouldStopAfterTurn ends the run after a turn that would otherwise
	// continue, consulted at the save point — see spec §2.3c.
	ShouldStopAfterTurn func(ctx context.Context, tr *TurnResult) (bool, error)

	// ToolOutputLimit caps, in bytes, the text a tool result sends to the model.
	// Zero means DefaultToolOutputLimit, negative means no cap — see spec §2.7b.
	ToolOutputLimit int
}

// ObserveOptions configures tracing for a run.
type ObserveOptions struct {

	// Tracer records a trace of the run; build one with tracing.NewTracer(processor).
	Tracer *tracing.Tracer

	// IncludeSensitiveData decides whether generation spans record the model
	// request and output items; nil means include — see spec §2.14.
	IncludeSensitiveData *bool

	// TraceGroupID links this run's trace to related traces (one chat thread
	// across runs). Used only when Tracer starts a new trace.
	TraceGroupID string

	// TraceMetadata attaches user metadata to a trace Tracer starts.
	TraceMetadata map[string]any
}
