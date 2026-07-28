package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
)

// AgentToolConfig configures Agent.AsTool and AgentAsTool.
type AgentToolConfig struct {
	// Name is the tool name exposed to the calling agent. Defaults to the
	// agent's name (sanitized) when empty.
	Name string
	// Description tells the calling model what the tool does and when to use it.
	Description string
	// MaxTurns bounds the nested run. Zero uses DefaultMaxTurns.
	MaxTurns int
	// CustomOutputExtractor, when set, derives the tool's string result from the
	// nested run. By default the final output is used.
	CustomOutputExtractor func(*RunResult) (string, error)

	// IsEnabled, when non-nil, is consulted before exposing the tool to the
	// calling model; returning false hides it for that run. The counterpart of
	// Python's as_tool(is_enabled=...).
	IsEnabled func(ctx context.Context, rc *RunContext, agent *Agent) (bool, error)

	// NeedsApproval pauses the parent run before the nested agent executes,
	// surfacing a ToolApprovalItem for a human to approve or reject — the agent
	// tool itself becomes the approval gate. Use NeedsApprovalFunc for per-call
	// decisions (it takes precedence).
	NeedsApproval     bool
	NeedsApprovalFunc func(ctx context.Context, rc *RunContext, argsJSON, callID string) (bool, error)

	// FailureErrorFunction overrides how a failed nested run is rendered back
	// to the calling model. nil keeps DefaultToolErrorFunction. To make
	// failures fatal instead (Python's failure_error_function=None), clear the
	// field on the returned *FunctionTool.
	FailureErrorFunction func(ctx context.Context, tc *ToolContext, err error) string

	// Session gives the nested run conversation state of its own. The parent
	// run's Session is never inherited — to share history, pass the same
	// Session here explicitly.
	Session *Session

	// ConversationID attaches the nested run to a server-side OpenAI
	// conversation. Like a top-level run, the nested run can use only one
	// state strategy at a time (Session or server-managed state). Ignored when
	// resuming a paused nested run — the serialized state already carries the
	// conversation so far.
	ConversationID string

	// ModifyRunOptions, when non-nil, is applied last to the computed nested
	// RunOptions — the Go counterpart of Python's as_tool(run_config=...)
	// override. Use it to adjust anything the dedicated fields don't cover
	// (model, guardrails, filters, ...).
	ModifyRunOptions func(*RunOptions)

	// OnStream, when non-nil, switches the nested run to streaming and
	// delivers every stream event to the callback, mirroring Python's
	// as_tool(on_stream=...). Events are dispatched from a single background
	// goroutine so a slow callback does not stall the nested run. When the
	// nested run completes normally the tool call waits for the callback to
	// drain; when the parent run is canceled it does not. A panic inside the
	// callback is recovered and dropped — a handler bug never fails the call.
	OnStream func(AgentToolStreamEvent)

	// InputBuilder, when non-nil, renders the tool's JSON arguments into the
	// nested run's input text, replacing the default rendering. See
	// DefaultAgentToolInputBuilder for the default applied when a structured
	// schema (AgentAsTool) is in play.
	InputBuilder AgentToolInputBuilder

	// IncludeInputSchema attaches the full parameters JSON schema to the
	// default structured-input rendering (AgentAsTool only; ignored without
	// structured parameters, matching Python's include_input_schema).
	IncludeInputSchema bool
}

// AgentToolStreamEvent is delivered to AgentToolConfig.OnStream for every
// stream event of a nested agent-as-tool run. It mirrors Python's
// AgentToolStreamEvent.
type AgentToolStreamEvent struct {
	// Event is the nested run's stream event.
	Event StreamEvent
	// Agent is the nested agent currently emitting events; it follows handoffs
	// inside the nested run (tracked via AgentUpdatedStreamEvent).
	Agent *Agent
	// ToolCallID, ToolName and Arguments identify the originating tool call in
	// the parent run.
	ToolCallID string
	ToolName   string
	Arguments  string
}

// AgentToolInvocation identifies the parent tool call that produced a nested
// agent-as-tool run. It is exposed on the nested RunResult so a
// CustomOutputExtractor can tell which call it is extracting for — the
// counterpart of Python's RunResult.agent_tool_invocation.
type AgentToolInvocation struct {
	ToolName   string
	ToolCallID string
	Arguments  string
}

// agentToolInput is the default argument schema for an agent tool: a single
// input string forwarded to the nested agent.
type agentToolInput struct {
	Input string `json:"input" jsonschema:"The input to pass to the agent"`
}

// AsTool turns the agent into a Tool callable by other agents. Unlike a handoff,
// the nested agent receives only the provided input (not the full conversation)
// and returns control to the calling agent when done. It mirrors the Python
// SDK's Agent.as_tool.
//
// The nested run inherits the parent run's model provider, model override,
// model settings, run-level guardrails and tracer from the run context, so the
// sub-agent need not set its own model when the parent supplies a provider.
// For a custom argument schema, use AgentAsTool.
func (a *Agent) AsTool(cfg AgentToolConfig) Tool {
	schema, err := SchemaFor[agentToolInput](true)
	if err != nil {
		schema = emptyStrictSchema()
	}
	return agentTool(a, cfg, schema, agentToolSchemaInfo{}, nil)
}

// AgentAsTool is AsTool with a custom argument schema: the tool's parameters
// are reflected from Params (like NewFunctionTool), and the arguments are
// rendered into the nested run's input with the default structured rendering
// (preamble + JSON + schema summary; see DefaultAgentToolInputBuilder) or
// cfg.InputBuilder. It mirrors Python's as_tool(parameters=...). A free
// function because Go methods cannot take type parameters.
func AgentAsTool[Params any](a *Agent, cfg AgentToolConfig) Tool {
	name := cfg.Name
	if name == "" {
		name = transformToolName(a.Name)
	}
	schema, err := SchemaFor[Params](true)
	if err != nil {
		return failedFunctionTool(name, cfg.Description, fmt.Errorf("agent tool %q: %w", name, err))
	}
	// Arguments get the same whole-schema validation NewFunctionTool gives
	// its args (spec: "validated against the whole JSON Schema") — a bare
	// decode caught type mismatches but let a missing required key or a
	// violated enum flow straight into the nested run's input.
	validator := newSchemaValidator(schema)
	validate := func(argsJSON string) error {
		var params Params
		return decodeToolArgs(name, validator, argsJSON, &params)
	}
	return agentTool(a, cfg, schema, buildStructuredSchemaInfo(schema, cfg.IncludeInputSchema), validate)
}

// agentTool builds the FunctionTool shared by AsTool and AgentAsTool.
// validate, when non-nil, type-checks the raw arguments (AgentAsTool's Params
// decode); nil falls back to the default {"input": string} handling.
func agentTool(a *Agent, cfg AgentToolConfig, schema map[string]any, info agentToolSchemaInfo, validate func(string) error) Tool {
	name := cfg.Name
	if name == "" {
		name = transformToolName(a.Name)
	}
	failureFn := cfg.FailureErrorFunction
	if failureFn == nil {
		failureFn = DefaultToolErrorFunction
	}
	return &FunctionTool{
		Name:                 name,
		Description:          cfg.Description,
		ParamsJSONSchema:     schema,
		Strict:               true,
		FailureErrorFunction: failureFn,
		IsEnabled:            cfg.IsEnabled,
		NeedsApproval:        cfg.NeedsApproval,
		NeedsApprovalFunc:    cfg.NeedsApprovalFunc,
		OnInvoke: func(ctx context.Context, tc *ToolContext, argsJSON string) (ToolResult, error) {
			nestedOpts := nestedRunOptions(tc.RunContext, cfg)
			if cfg.ModifyRunOptions != nil {
				cfg.ModifyRunOptions(&nestedOpts)
			}
			// Parent the nested run's agent spans under this tool call's
			// function span so the trace tree shows which call owns the run.
			nestedOpts.parentSpanID = tc.functionSpanID

			var res *RunResult
			var err error
			resumed := false
			// On resume, continue the nested run this call paused (human-in-the-
			// loop): its state was cached on the parent run context by tool call
			// id. Mirror the parent's approve/reject decisions for the surfaced
			// nested interruptions into the nested run before resuming so the
			// human's choice takes effect. Mirrors Python's as_tool resume path.
			if tc.RunContext != nil {
				if paused := tc.takeNestedToolState(tc.ToolCallID); paused != nil {
					resumed = true
					if tc.Approvals != nil {
						tc.Approvals.mirrorInto(paused.Approvals, paused.Interruptions)
					}
					resumeOpts := nestedOpts
					// The serialized state already carries the conversation so
					// far (Python nulls conversation_id/previous_response_id on
					// resume).
					resumeOpts.Conversation.ConversationID = ""
					res, err = runNestedAgent(ctx, a, "", paused, resumeOpts, cfg, tc, argsJSON)
				}
			}
			if !resumed {
				// Arguments validate against their declared shape before they
				// can influence the nested run — mirroring the Python
				// TypeAdapter: a malformed-argument error goes back to the
				// model to self-correct instead of silently becoming the
				// nested run's prompt.
				switch {
				case validate != nil:
					if uerr := validate(argsJSON); uerr != nil {
						return ToolResult{}, newModelBehaviorError("agent tool %q: invalid arguments: %v", name, uerr)
					}
				case cfg.InputBuilder == nil && !info.structured:
					var args agentToolInput
					if uerr := json.Unmarshal([]byte(argsJSON), &args); uerr != nil {
						return ToolResult{}, newModelBehaviorError("agent tool %q: invalid arguments: %v", name, uerr)
					}
				default:
					if !json.Valid([]byte(argsJSON)) {
						return ToolResult{}, newModelBehaviorError("agent tool %q: invalid arguments: not valid JSON", name)
					}
				}
				input, ierr := resolveAgentToolInput(argsJSON, info, cfg.InputBuilder)
				if ierr != nil {
					return ToolResult{}, fmt.Errorf("agent tool %q: building input: %w", name, ierr)
				}
				res, err = runNestedAgent(ctx, a, input, nil, nestedOpts, cfg, tc, argsJSON)
			}
			if err != nil {
				return ToolResult{}, fmt.Errorf("agent tool %q run failed: %w", name, err)
			}
			// The nested run paused for approval: surface its interruptions to the
			// parent run (via the sentinel below) instead of returning an output,
			// so the parent pauses too and the human sees the nested approval.
			// Usage is not folded in yet — it will be when the resumed nested run
			// finally completes, carrying its full usage.
			if len(res.Interruptions) > 0 {
				return ToolResult{}, &nestedRunInterrupt{
					callID:        tc.ToolCallID,
					state:         res.State,
					interruptions: res.Interruptions,
				}
			}
			// Fold the completed nested run's usage into the parent run's usage
			// (Python parity: the nested run shares the parent's usage). Add is
			// goroutine-safe, so concurrent agent-tool calls are fine.
			if tc.RunContext != nil && res.Usage != nil {
				tc.Usage.Add(res.Usage)
			}
			res.AgentToolInvocation = &AgentToolInvocation{
				ToolName:   tc.ToolName,
				ToolCallID: tc.ToolCallID,
				Arguments:  argsJSON,
			}
			out := agentToolOutput(res)
			if cfg.CustomOutputExtractor != nil {
				custom, cerr := cfg.CustomOutputExtractor(res)
				if cerr != nil {
					return ToolResult{}, cerr
				}
				out = custom
			}
			// Report the nested run's usage on the result so it is attributable
			// to THIS tool call, not just folded into the run total where
			// nothing says which call spent it.
			result := resultFromValue(out)
			result.Usage = res.Usage
			return result, nil
		},
	}
}

// runNestedAgent executes (or resumes) the nested run. Without an OnStream
// handler it collects; with one, it forwards each event to the handler as it
// arrives.
func runNestedAgent(ctx context.Context, a *Agent, input string, paused *RunState, opts RunOptions, cfg AgentToolConfig, tc *ToolContext, argsJSON string) (*RunResult, error) {
	// Stream the nested run when anyone is watching: a configured OnStream
	// handler, or a streamed parent whose consumer should see the sub-agent
	// working rather than a tool call that hangs for a minute.
	if cfg.OnStream == nil && !tc.streaming() {
		if paused != nil {
			return ResumeRunSync(ctx, paused, opts)
		}
		return RunSync(ctx, a, input, opts)
	}

	var stream RunStream
	if paused != nil {
		stream, _ = ResumeRun(ctx, paused, opts)
	} else {
		stream, _ = Run(ctx, a, input, opts)
	}

	// Dispatch callbacks from a background goroutine so a slow handler does not
	// stall the run — the stream now runs on THIS goroutine, so a blocking
	// handler would hold up the nested run itself.
	//
	// canceled mirrors the parent's cancellation: once it fires, backlogged
	// events drain without invoking the callback, so OnStream never fires after
	// the tool call has returned.
	var canceled atomic.Bool
	events := make(chan AgentToolStreamEvent, 64)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range events {
			if canceled.Load() {
				continue
			}
			dispatchAgentToolStreamEvent(cfg.OnStream, ev)
		}
	}()

	current := a
	var res *RunResult
	var runErr error
	for ev, eerr := range stream {
		if eerr != nil {
			runErr = eerr
			break
		}
		switch e := ev.(type) {
		case *AgentUpdatedStreamEvent:
			if e.NewAgent != nil {
				current = e.NewAgent
			}
		case *RunCompletedEvent:
			res = e.Result
			// The completion is the loop's own bookkeeping, not something a
			// handler watching the nested agent should see.
			continue
		case *RunItemStreamEvent:
			// Forward the nested agent's messages to the PARENT run's stream as
			// tool progress, so a UI watching the parent sees the sub-agent
			// working without the caller wiring OnStream. Messages only: the
			// raw deltas belong to the nested run, and relaying them would
			// bury the parent's own stream.
			if m, ok := e.Item.(*MessageOutputItem); ok {
				tc.Emit(TextResult(m.Text()).WithDetails(map[string]any{
					"nested_agent": current.Name, "partial": true,
				}))
			}
		}
		if ctx.Err() != nil {
			// Parent canceled: keep draining so the run can finish and record
			// its result, but stop dispatching to the handler.
			canceled.Store(true)
			continue
		}
		if cfg.OnStream == nil {
			// Streaming only to forward progress; there is no handler to feed.
			continue
		}
		payload := AgentToolStreamEvent{
			Event:      ev,
			Agent:      current,
			ToolCallID: tc.ToolCallID,
			ToolName:   tc.ToolName,
			Arguments:  argsJSON,
		}
		select {
		case events <- payload:
		case <-ctx.Done():
		}
	}
	if ctx.Err() != nil {
		canceled.Store(true)
	}
	close(events)
	if ctx.Err() == nil {
		// Normal completion waits for the handler backlog to drain;
		// cancellation does not (the canceled flag makes the leftover drain a
		// no-op).
		<-done
	}
	if res == nil && runErr == nil {
		// The stream ended without completing — cancellation.
		runErr = ctx.Err()
	}
	return res, runErr
}

// dispatchAgentToolStreamEvent invokes the OnStream callback, recovering any
// panic: a handler bug must not fail the tool call (Python logs and continues;
// the Go SDK has no logger, so the panic is dropped).
func dispatchAgentToolStreamEvent(fn func(AgentToolStreamEvent), ev AgentToolStreamEvent) {
	defer func() { _ = recover() }()
	fn(ev)
}

// nestedRunInterrupt is returned by an agent-as-tool's OnInvoke when its nested
// run paused for human approval. The parent runner recognizes it (rather than
// treating it as a tool failure), surfaces the nested interruptions as the
// parent run's own, and caches state under callID so a ResumeRun continues the
// nested run.
type nestedRunInterrupt struct {
	callID        string
	state         *RunState
	interruptions []*ToolApprovalItem
}

func (e *nestedRunInterrupt) Error() string {
	return fmt.Sprintf("agent tool call %q paused for nested approval", e.callID)
}

// agentToolOutput derives the string result of a completed nested run,
// mirroring Python's as_tool default extraction: the final output when
// non-empty, else the last non-empty assistant message text, else the last
// non-empty string tool output, else the (stringified) final output.
func agentToolOutput(res *RunResult) string {
	if s, ok := res.FinalOutput.(string); ok {
		if s != "" {
			return s
		}
	} else if res.FinalOutput != nil {
		// A structured final output: render it as its JSON payload rather than a
		// Go %v rendering, so the calling model receives valid data.
		if b, err := json.Marshal(res.FinalOutput); err == nil {
			return string(b)
		}
		return res.FinalOutputString()
	}
	for i := len(res.NewItems) - 1; i >= 0; i-- {
		switch it := res.NewItems[i].(type) {
		case *MessageOutputItem:
			if t := it.Text(); t != "" {
				return t
			}
		case *ToolCallOutputItem:
			if s, ok := it.Output.(string); ok && s != "" {
				return s
			}
		}
	}
	return res.FinalOutputString()
}

// nestedRunOptions builds the RunOptions for a nested run, inheriting the
// parent's model provider/model/tracer, run-level guardrails and side-effect-
// free execution options (tool concurrency limit, tool-not-found behavior,
// input filters) — the Go counterpart of Python's "a nested run with
// run_config=None reuses the parent's RunConfig". The nested run gets a fresh
// approval store so nested approvals don't leak into the parent. The parent's
// Session and Hooks never carry over; cfg.Session / cfg.Hooks are the only way
// to give the nested run state and lifecycle callbacks of its own.
func nestedRunOptions(parent *RunContext, cfg AgentToolConfig) RunOptions {
	opts := RunOptions{Exec: ExecOptions{MaxTurns: cfg.MaxTurns}}
	if parent != nil && parent.inheritedOpts != nil {
		opts.Model.Provider = parent.inheritedOpts.Model.Provider
		opts.Model.Override = parent.inheritedOpts.Model.Override
		opts.Model.Settings = parent.inheritedOpts.Model.Settings
		opts.Observe.Tracer = parent.inheritedOpts.Observe.Tracer
		opts.Exec.MaxToolConcurrency = parent.inheritedOpts.Exec.MaxToolConcurrency
		opts.Exec.ToolNotFoundBehavior = parent.inheritedOpts.Exec.ToolNotFoundBehavior
		opts.Exec.PreApprovalToolInputGuardrails = parent.inheritedOpts.Exec.PreApprovalToolInputGuardrails
		// Inherit the sensitive-data gate so a parent that disabled span
		// content cannot have it re-enabled by a nested agent-as-tool run.
		opts.Observe.IncludeSensitiveData = parent.inheritedOpts.Observe.IncludeSensitiveData
		// Inherit input filters so a nested run's own handoffs and model calls
		// see the same rewriting the parent configured.
		opts.Exec.HandoffInputFilter = parent.inheritedOpts.Exec.HandoffInputFilter
		opts.Model.InputFilter = parent.inheritedOpts.Model.InputFilter
		// Inherit run-level guardrails so a nested run is guarded like its parent.
		opts.Guardrails = parent.inheritedOpts.Guardrails
	}
	if parent != nil {
		// Join the parent's trace so the nested run's spans are attributed to
		// the same workflow instead of an orphan root trace.
		opts.parentTrace = parent.activeTrace
	}
	// Share the parent's user context value, but use a fresh usage accumulator
	// and approval store for the nested run.
	if parent != nil {
		opts.Context = parent.Context
	}
	opts.Conversation.Session = cfg.Session
	opts.Conversation.ConversationID = cfg.ConversationID
	return opts
}
