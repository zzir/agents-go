package agents

import (
	"context"
	"fmt"
)

// AgentToolConfig configures Agent.AsTool: the tool surface; the nested run is
// ModifyRunOptions's job — see decisions §5.13.
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
}

// agentToolInput is the argument schema: one input string forwarded to the agent.
type agentToolInput struct {
	Input string `json:"input" jsonschema:"The input to pass to the agent"`
}

// AsTool turns the agent into a Tool callable by other agents: the nested run
// takes the single `input` string verbatim and returns control when done — see
// spec §2.8 and decisions §5.110.
func (a *Agent) AsTool(cfg AgentToolConfig) *Tool {
	name := cfg.Name
	if name == "" {
		name = transformToolName(a.Name)
	}
	schema, err := SchemaFor[agentToolInput](true)
	if err != nil {
		panic(fmt.Sprintf("agents: AsTool(%q): schema generation failed: %v", a.Name, err))
	}
	validator := newSchemaValidator(schema)
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
					res, err = runNestedAgent(ctx, a, "", paused, resumeOpts, tc)
				}
			}
			if !resumed {
				// Whole-schema check first; a violation is a
				// *ModelBehaviorError — see spec §2.7h.
				var args agentToolInput
				if verr := decodeToolArgs(name, validator, argsJSON, &args); verr != nil {
					return ToolResult{}, verr
				}
				res, err = runNestedAgent(ctx, a, args.Input, nil, nestedOpts, tc)
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
