package agents

import (
	"context"
	"fmt"
)

// AgentToolConfig configures Agent.AsTool and AgentAsTool: the TOOL surface —
// name, visibility, approval, error rendering, argument rendering. The nested
// RUN is ModifyRunOptions's job (decisions §5.13).
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
	// calling model; returning false hides it for that run.
	IsEnabled func(ctx context.Context, rc *RunContext, agent *Agent) (bool, error)

	// NeedsApproval pauses the parent run before the nested agent executes,
	// surfacing a ToolApprovalItem — the agent tool is the approval gate.
	// NeedsApprovalFunc decides per call and takes precedence.
	NeedsApproval     bool
	NeedsApprovalFunc func(ctx context.Context, rc *RunContext, argsJSON, callID string) (bool, error)

	// FailureErrorFunction overrides how a failed nested run is rendered back to
	// the calling model. nil keeps DefaultToolErrorFunction. To make failures
	// fatal instead, clear the field on the returned *Tool.
	FailureErrorFunction func(ctx context.Context, tc *ToolContext, err error) string

	// ModifyRunOptions edits the nested run's RunOptions before it starts —
	// the one channel for run-level configuration (a Session of its own,
	// Exec.MaxTurns, a conversation, models, guardrails), applied over what is
	// inherited (see nestedRunOptions). The nested run has no Session unless
	// set here; a ConversationID set here is cleared when a paused run resumes.
	ModifyRunOptions func(*RunOptions)

	// OnStream, when non-nil, streams the nested run and delivers every event
	// to the callback from one background goroutine, so a slow callback does
	// not stall the run. Normal completion drains the callback; cancellation
	// does not. A panic inside the callback is recovered and dropped.
	OnStream func(AgentToolStreamEvent)

	// InputBuilder, when non-nil, renders the tool's JSON arguments into the
	// nested run's input text in place of DefaultAgentToolInputBuilder; set
	// AgentToolInputWithSchema to attach the full parameters schema.
	InputBuilder AgentToolInputBuilder
}

// AgentToolStreamEvent is delivered to AgentToolConfig.OnStream for every
// stream event of a nested agent-as-tool run.
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
// CustomOutputExtractor can tell which call it is extracting for.
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

// AsTool turns the agent into a Tool callable by other agents. Unlike a
// handoff, the nested agent receives only the provided input and returns
// control to the caller when done. The tool takes a single `input` string,
// which becomes the nested run's input verbatim; AgentAsTool takes a custom
// schema. The nested run inherits the parent's model provider, override,
// settings, run-level guardrails and tracer.
func (a *Agent) AsTool(cfg AgentToolConfig) *Tool {
	name := agentToolName(a, cfg)
	schema, err := SchemaFor[agentToolInput](true)
	if err != nil {
		// agentToolInput is a fixed struct; its schema cannot fail to reflect.
		panic(fmt.Sprintf("agents: AsTool(%q): schema generation failed: %v", a.Name, err))
	}
	validator := newSchemaValidator(schema)
	validate := func(argsJSON string) error {
		var args agentToolInput
		return decodeToolArgs(name, validator, argsJSON, &args)
	}
	return agentTool(a, cfg, schema, agentToolSchemaInfo{}, validate)
}

// AgentAsTool is AsTool with a custom argument schema reflected from Params
// (like NewTool); the arguments are rendered into the nested run's input by
// DefaultAgentToolInputBuilder or cfg.InputBuilder. A free function because Go
// methods cannot take type parameters.
func AgentAsTool[Params any](a *Agent, cfg AgentToolConfig) *Tool {
	name := agentToolName(a, cfg)
	schema, err := SchemaFor[Params](true)
	if err != nil {
		panic(fmt.Sprintf("agents: AgentAsTool(%q): schema generation failed: %v", name, err))
	}
	validator := newSchemaValidator(schema)
	validate := func(argsJSON string) error {
		var params Params
		return decodeToolArgs(name, validator, argsJSON, &params)
	}
	return agentTool(a, cfg, schema, buildStructuredSchemaInfo(schema), validate)
}

// agentToolName resolves an agent tool's name: the configured one, or the
// agent's own name sanitized into a tool name.
func agentToolName(a *Agent, cfg AgentToolConfig) string {
	if cfg.Name != "" {
		return cfg.Name
	}
	return transformToolName(a.Name)
}

// agentTool builds the Tool shared by AsTool and AgentAsTool. validate checks
// the raw arguments against the schema the tool advertises.
func agentTool(a *Agent, cfg AgentToolConfig, schema map[string]any, info agentToolSchemaInfo, validate func(string) error) *Tool {
	name := agentToolName(a, cfg)
	failureFn := cfg.FailureErrorFunction
	if failureFn == nil {
		failureFn = DefaultToolErrorFunction
	}
	return &Tool{
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
			// On resume, continue the nested run this call paused, mirroring the
			// parent's approve/reject decisions into it first.
			if tc.RunContext != nil {
				if paused := tc.takeNestedToolState(tc.ToolCallID); paused != nil {
					resumed = true
					if tc.Approvals != nil {
						tc.Approvals.mirrorInto(paused.Approvals, paused.Interruptions)
					}
					resumeOpts := nestedOpts
					// The serialized state already carries the conversation so far.
					resumeOpts.Conversation.ConversationID = ""
					res, err = runNestedAgent(ctx, a, "", paused, resumeOpts, cfg, tc, argsJSON)
				}
			}
			if !resumed {
				// Arguments face the whole-schema check first (spec §2.7h): a
				// violation is a *ModelBehaviorError, not the nested run's prompt.
				if verr := validate(argsJSON); verr != nil {
					return ToolResult{}, verr
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
			// A paused nested run surfaces its interruptions to the parent via the
			// sentinel; usage folds in when the resumed run completes.
			if len(res.Interruptions) > 0 {
				return ToolResult{}, &nestedRunInterrupt{
					callID:        tc.ToolCallID,
					state:         res.State,
					interruptions: res.Interruptions,
				}
			}
			// Fold the completed nested run's usage into the parent run's usage. Add is
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
			// to THIS tool call, not just folded into the run total.
			result := resultFromValue(out)
			result.Usage = res.Usage
			return result, nil
		},
	}
}
