// Package agents is a Go SDK for building agentic applications on top of large
// language models, ported from openai-agents-python (tracking v0.17.4).
//
// An [Agent] pairs a model with instructions, tools, handoffs, guardrails and an
// optional structured output type. [Run] drives the agent loop until the agent
// produces a final output, hands off to another agent that finishes, or the turn
// budget is exhausted; [RunStreamed] does the same while streaming events.
//
// # Tools
//
// Build a function tool from a typed Go function with [NewFunctionTool]; the
// argument struct is reflected into a strict JSON schema shown to the model.
// An agent can also be exposed as a tool via [Agent.AsTool].
//
// # Structured output
//
// Set [Agent.OutputType] to [OutputType][T] to have the model return a validated
// value of type T; recover it with [FinalOutputAs].
//
// # Handoffs
//
// [HandoffTo] builds a handoff that transfers control to another agent.
//
// # Guardrails
//
// Input, output and tool-level guardrails ([InputGuardrail], [OutputGuardrail],
// [ToolInputGuardrail], [ToolOutputGuardrail]) can halt a run or substitute tool
// content when a tripwire fires.
//
// # Sessions
//
// A [Session] persists conversation history across runs. [InMemorySession] is
// built in; see the memory subpackage for a SQLite-backed implementation.
//
// # Human-in-the-loop
//
// Mark a tool with NeedsApproval to pause the run before it executes. The paused
// [RunState] (on [RunResult].State) can be approved or rejected and resumed with
// [ResumeRun], and serialized to JSON for cross-process approval flows.
//
// # Models
//
// Implementations of the [Model] interface live in provider subpackages; the
// openai subpackage targets the OpenAI Responses API.
package agents
