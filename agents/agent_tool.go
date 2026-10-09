package agents

import (
	"context"
	"fmt"
)

// AgentToolConfig configures Agent.AsTool and AgentAsTool: the tool surface;
// the nested run is ModifyRunOptions's job — see decisions §5.13.
type AgentToolConfig struct {
	// Name is the tool name exposed to the calling agent; defaults to the
	// agent's name, sanitized.
	Name string
	// Description tells the calling model what the tool does and when to use it.
	Description string
	// CustomOutputExtractor derives the tool's string result from the nested
	// run; the default is the final output.
	CustomOutputExtractor func(*RunResult) (string, error)

	// IsEnabled, when non-nil, hides the tool from the calling model on false.
	IsEnabled func(ctx context.Context, rc *RunContext, agent *Agent) (bool, error)

	// NeedsApproval pauses the parent run before the nested agent executes;
	// NeedsApprovalFunc decides per call and takes precedence.
	NeedsApproval     bool
	NeedsApprovalFunc func(ctx context.Context, rc *RunContext, argsJSON, callID string) (bool, error)

	// FailureErrorFunction renders a failed nested run back to the calling
	// model; nil keeps DefaultToolErrorFunction. Clear it on the *Tool for fatal.
	FailureErrorFunction func(ctx context.Context, tc *ToolContext, err error) string

	// ModifyRunOptions edits the nested run's RunOptions, applied over what it
	// inherits (nestedRunOptions). No Session unless set here.
	ModifyRunOptions func(*RunOptions)

	// OnStream streams the nested run to the callback from one background
	// goroutine; completion drains it, cancellation does not, a panic is dropped.
	OnStream func(AgentToolStreamEvent)

	// InputBuilder renders the tool's JSON arguments into the nested run's
	// input text in place of DefaultAgentToolInputBuilder.
	InputBuilder AgentToolInputBuilder
}

// AgentToolStreamEvent is delivered to AgentToolConfig.OnStream for every
// stream event of a nested agent-as-tool run.
type AgentToolStreamEvent struct {
	// Event is the nested run's stream event.
	Event StreamEvent
	// Agent is the nested agent currently emitting; it follows handoffs inside
	// the nested run.
	Agent *Agent
	// ToolCallID, ToolName and Arguments identify the originating tool call in
	// the parent run.
	ToolCallID string
	ToolName   string
	Arguments  string
}

// AgentToolInvocation identifies the parent tool call that produced a nested
// agent-as-tool run, exposed on the nested RunResult.
type AgentToolInvocation struct {
	ToolName   string
	ToolCallID string
	Arguments  string
}

// agentToolInput is the default argument schema: one input string forwarded to
// the agent.
type agentToolInput struct {
	Input string `json:"input" jsonschema:"The input to pass to the agent"`
}

// AsTool turns the agent into a Tool callable by other agents: the nested run
// takes the single `input` string verbatim and returns control when done
// (AgentAsTool takes a custom schema) — see spec §2.8.
func (a *Agent) AsTool(cfg AgentToolConfig) *Tool {
	name := agentToolName(a, cfg)
	schema, err := SchemaFor[agentToolInput](true)
	if err != nil {
		panic(fmt.Sprintf("agents: AsTool(%q): schema generation failed: %v", a.Name, err))
	}
	validator := newSchemaValidator(schema)
	validate := func(argsJSON string) error {
		var args agentToolInput
		return decodeToolArgs(name, validator, argsJSON, &args)
	}
	return agentTool(a, cfg, schema, agentToolSchemaInfo{}, validate)
}

// AgentAsTool is AsTool with an argument schema reflected from Params; the
// arguments are rendered into the nested run's input by cfg.InputBuilder or
// DefaultAgentToolInputBuilder.
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

// agentToolName resolves an agent tool's name: cfg.Name, else the agent's name
// sanitized.
func agentToolName(a *Agent, cfg AgentToolConfig) string {
	if cfg.Name != "" {
		return cfg.Name
	}
	return transformToolName(a.Name)
}

// agentTool builds the Tool shared by AsTool and AgentAsTool; validate checks
// the raw arguments against the advertised schema.
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
			// Parent the nested run's agent spans under this call's function span.
			nestedOpts.parentSpanID = tc.functionSpanID

			var res *RunResult
			var err error
			resumed := false
			// On resume, continue the nested run this call paused, mirroring the
			// parent's approvals into it first.
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
				// Whole-schema check first; a violation is a
				// *ModelBehaviorError — see spec §2.7h.
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
			// A paused nested run surfaces its interruptions to the parent via
			// the sentinel.
			if len(res.Interruptions) > 0 {
				return ToolResult{}, &nestedRunInterrupt{
					callID:        tc.ToolCallID,
					state:         res.State,
					interruptions: res.Interruptions,
				}
			}
			// Fold the nested run's usage into the parent's; Add is goroutine-safe.
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
			// The result carries the nested usage too, attributed to this call.
			result := resultFromValue(out)
			result.Usage = res.Usage
			return result, nil
		},
	}
}
