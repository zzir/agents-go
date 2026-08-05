package agents

import "github.com/zzir/agents-go/agents/session"

// The value types shared between a live run and its stored history live in
// the session package — entries persist them, so they sit with the stored
// form — and are aliased here because they are equally part of the runner's
// own surface: every RunItem carries a Source and projects a Display, every
// result reports RequestUsage, every survived failure is a Diagnostic. An
// alias is transparent: agents.Source IS session.Source, one type under two
// import paths.
//
// Session-only names (session.Entry, session.Storage, session.Session, …) are
// deliberately NOT aliased: code that works with stored history should import
// the package that owns it.
type (
	// Source records who produced an item or entry. The zero value is the model.
	Source = session.Source
	// SourceType is the enumeration behind Source.
	SourceType = session.SourceType
	// ItemDisplay is an item projected into what a renderer needs.
	ItemDisplay = session.Display
	// RequestUsage is one model request's token accounting.
	RequestUsage = session.RequestUsage
	// InputTokensDetails breaks down a request's input tokens.
	InputTokensDetails = session.InputTokensDetails
	// OutputTokensDetails breaks down a request's output tokens.
	OutputTokensDetails = session.OutputTokensDetails
	// Diagnostic is trouble a run survived, recorded rather than raised.
	Diagnostic = session.Diagnostic
	// DiagnosticType classifies a Diagnostic.
	DiagnosticType = session.DiagnosticType
)

// Source types, re-exported alongside the Source alias.
const (
	SourceModel        = session.SourceModel
	SourceUser         = session.SourceUser
	SourceTool         = session.SourceTool
	SourceHandoff      = session.SourceHandoff
	SourceGuardrail    = session.SourceGuardrail
	SourceCompaction   = session.SourceCompaction
	SourceErrorHandler = session.SourceErrorHandler
)

// Display kinds, re-exported alongside the ItemDisplay alias.
const (
	DisplayMessage    = session.DisplayMessage
	DisplayToolCall   = session.DisplayToolCall
	DisplayToolOutput = session.DisplayToolOutput
	DisplayReasoning  = session.DisplayReasoning
	DisplayHandoff    = session.DisplayHandoff
	DisplayUnknown    = session.DisplayUnknown
	DisplayError      = session.DisplayError
	DisplayCancelled  = session.DisplayCancelled
)

// Diagnostic types, re-exported alongside the Diagnostic alias.
const (
	DiagModelRetry        = session.DiagModelRetry
	DiagModelFallback     = session.DiagModelFallback
	DiagStreamError       = session.DiagStreamError
	DiagToolPanic         = session.DiagToolPanic
	DiagToolTimeout       = session.DiagToolTimeout
	DiagCompactionFailed  = session.DiagCompactionFailed
	DiagContextOverflow   = session.DiagContextOverflow
	DiagResponseTruncated = session.DiagResponseTruncated
)
