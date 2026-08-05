// Package agents is a Go SDK for building agent applications on the OpenAI
// Responses API: a run loop with tools, handoffs, guardrails, sessions,
// human-in-the-loop approval and tracing.
//
// It began as a port of openai-agents-python and now evolves independently.
// Behavior is specified in docs/spec.md, not inherited; docs/architecture.md
// explains how the pieces compose.
//
// The package is layered. An agent and a run are the whole floor:
//
//	agent := &agents.Agent{
//		Name:         "assistant",
//		Instructions: agents.StaticInstructions("Be brief."),
//	}
//	res, err := agents.RunSync(ctx, agent, "hello", agents.RunOptions{
//		Model: agents.ModelOptions{Provider: openai.NewProvider()},
//	})
//
// Everything after that is an opt-in layer: tools when the model should act,
// a session when the conversation must persist, guardrails and approval when
// the stakes rise, middleware and tracing when runs need policy and
// observability. The sections below are ordered that way — each one builds on
// the previous but none is required by it.
//
// # Running
//
// An [Agent] is a plain struct pairing a model with instructions, tools,
// handoffs, guardrails and an optional structured output type. It has no Run
// method — everything happens in the runner, which takes the agent as data.
// [RunSync] executes a run and returns its [RunResult]. [Run] returns the
// same run as a [RunStream] to range over plus a [RunControl] to steer it:
// the run executes on the consumer's goroutine, ranging the stream advances
// the loop, and abandoning it stops the run where it stands.
//
// [RunOptions] configures a run; its zero value works as long as the agent
// can resolve a model. Cross-cutting policy — retrying a run, gating it on
// approvals, logging it — wraps the run as a [RunMiddleware]; the middleware
// subpackage ships the common ones.
//
// # Tools
//
// [NewTool] builds a tool from a typed Go function; the argument
// struct is reflected into a strict JSON schema shown to the model (chain
// [Tool.NonStrict] to relax it). Every tool executes locally:
// [Tool] is a struct rather than an interface, so provider-hosted
// tools have nowhere to be introduced. Optional behavior — approval,
// enablement, deferral, timeout, sequencing — is a field on it, and adapting
// a tool you did not build is copying the struct and assigning to one.
// [Agent.AsTool] exposes a whole agent as a callable tool.
//
// # Structured output
//
// Give the agent an OutputType built with [OutputType] to have the model
// return a validated value of type T; recover it with [FinalOutputAs].
//
// # Handoffs
//
// [HandoffTo] builds a handoff that transfers control to another agent; the
// run continues under the new agent until one of them finishes.
//
// # Guardrails
//
// A single [Guardrail] type covers four stages — [StageInput], [StageOutput],
// [StageToolInput], [StageToolOutput] — and one value can serve several. A
// tripwire halts the run; a Replace verdict substitutes content instead.
//
// # Sessions
//
// Conversation history lives in the session subpackage: a session.Session
// persists append-only session.Entry values forming a tree — a retry abandons
// a branch instead of deleting it, and compaction appends a checkpoint
// instead of rewriting. Which entry kinds reach the model is a projection
// decided by session.Projector. Storage is pluggable: an in-memory store is
// built in, the filesession package stores entries in JSONL files, and the
// sessions module adds SQLite and PostgreSQL. Wire it to a run through
// [ConversationOptions].
//
// # Human in the loop
//
// A tool marked as needing approval pauses the run: the [RunResult] carries a
// [RunState] whose interruptions can be approved or rejected and then resumed
// with [ResumeRun]. The state serializes to JSON, so a run can pause in one
// process and resume in another.
//
// # Models
//
// [Model] and [ModelProvider] are the backend seam; the models/openai
// subpackage implements them for the OpenAI Responses API. [NewRetryModel],
// [NewFallbackModel], [RouterProvider] and [NewStreamOnlyModel] add retry,
// failover, routing and stream-only backend adaptation as provider-agnostic
// decorators, never as run-loop changes.
//
// # Observability
//
// [ErrorCode] classifies failures for transport; [RunResult].Diagnostics
// lists trouble the run survived. The tracing package records spans (the
// tracing/otel module maps them to OpenTelemetry), and [LogConfig] turns on
// structured slog records, silent by default.
package agents
