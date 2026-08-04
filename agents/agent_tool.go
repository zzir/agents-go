package agents

import (
	"context"
	"encoding/json"
	"fmt"
)

// AgentToolConfig configures Agent.AsTool and AgentAsTool.
//
// It configures the TOOL surface: its name, visibility, approval gate, error
// rendering, and how arguments become nested-run input. Configuring the nested
// RUN — its session, turn budget, conversation, model, guardrails — is
// ModifyRunOptions's job; there is one channel for run options rather than a
// mirror field here for each.
type AgentToolConfig struct {
	// Name is the tool name exposed to the calling agent. Defaults to the
	// agent's name (sanitized) when empty.
	Name string
	// Description tells the calling model what the tool does and when to use it.
	Description string
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

	// ModifyRunOptions edits the nested run's RunOptions before it starts. It
	// is the one channel for run-level configuration — a Session of the
	// nested run's own, Exec.MaxTurns, a server-side conversation, model
	// overrides, guardrails — applied last, over the options inherited from
	// the parent run (see nestedRunOptions for what is inherited). Two
	// defaults worth knowing: the nested run has no Session unless one is set
	// here, and a Conversation.ConversationID set here is cleared when a
	// paused nested run resumes (the serialized state already carries the
	// conversation so far).
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
	// nested run's input text, replacing the default rendering
	// (DefaultAgentToolInputBuilder). Set AgentToolInputWithSchema to attach
	// the full parameters schema to the rendering.
	InputBuilder AgentToolInputBuilder
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
		// agentToolInput is a fixed struct; its schema cannot fail to reflect.
		panic(fmt.Sprintf("agents: AsTool(%q): schema generation failed: %v", a.Name, err))
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
		panic(fmt.Sprintf("agents: AgentAsTool(%q): schema generation failed: %v", name, err))
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
	return agentTool(a, cfg, schema, buildStructuredSchemaInfo(schema), validate)
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
			nestedOpts := nestedRunOptions(tc.RunContext)
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
						return ToolResult{}, NewModelBehaviorError("agent tool %q: invalid arguments: %v", name, uerr)
					}
				case cfg.InputBuilder == nil && !info.structured:
					var args agentToolInput
					if uerr := json.Unmarshal([]byte(argsJSON), &args); uerr != nil {
						return ToolResult{}, NewModelBehaviorError("agent tool %q: invalid arguments: %v", name, uerr)
					}
				default:
					if !json.Valid([]byte(argsJSON)) {
						return ToolResult{}, NewModelBehaviorError("agent tool %q: invalid arguments: not valid JSON", name)
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
