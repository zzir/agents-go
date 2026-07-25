package agents

import (
	"context"
	"time"
)

// Unwrapper is a tool wrapper that exposes what it wraps, so a capability
// lookup can see past it.
type Unwrapper interface {
	Unwrap() Tool
}

// ToolAs finds the first layer of a tool that implements T, walking through
// decorators.
//
// **It is the only correct way to ask a tool what it can do.** A bare type
// assertion compiles and silently returns false through a decorator: embedding
// the Tool interface promotes Tool's own methods and nothing else, so
// WithTimeout(WithApproval(t)).(ApprovalRequiredTool) is false — verified, not
// assumed. This walks the chain the way errors.As does.
func ToolAs[T any](t Tool) (T, bool) {
	for t != nil {
		if v, ok := t.(T); ok {
			return v, true
		}
		u, ok := t.(Unwrapper)
		if !ok {
			break
		}
		t = u.Unwrap()
	}
	var zero T
	return zero, false
}

// Optional tool capabilities. A tool declares one by implementing it; the
// runner asks with ToolAs.
type (
	// ApprovalRequiredTool pauses a run for human approval before it executes.
	ApprovalRequiredTool interface {
		NeedsToolApproval(ctx context.Context, rc *RunContext, argsJSON, callID string) (bool, error)
	}
	// GuardedTool carries guardrails for its own calls.
	GuardedTool interface {
		ToolGuardrails() []Guardrail
	}
	// TimeoutTool bounds a single invocation.
	TimeoutTool interface {
		ToolTimeout() time.Duration
	}
	// SequentialTool must not run concurrently with other tools in its turn.
	SequentialTool interface {
		RunsSequentially() bool
	}
	// EnableableTool decides per run whether the model sees it.
	EnableableTool interface {
		IsToolEnabled(ctx context.Context, rc *RunContext, agent *Agent) (bool, error)
	}
	// FailureHandlingTool turns its own errors into output the model can
	// recover from. A tool that does not implement it aborts the run on error.
	FailureHandlingTool interface {
		HandleToolFailure(ctx context.Context, tc *ToolContext, err error) string
	}
	// InvokableTool executes. Every tool the model can call implements it;
	// decorators do not, so a ToolAs lookup finds the layer that actually runs.
	InvokableTool interface {
		Invoke(ctx context.Context, tc *ToolContext, argsJSON string) (ToolResult, error)
	}
	// DescribableTool exposes what the model is told about a tool. Decorators
	// leave it to the layer beneath, so wrapping never changes the schema.
	DescribableTool interface {
		ToolDescription() string
		ToolParamsSchema() map[string]any
		ToolStrict() bool
	}
)

// deco is the shared shell every decorator embeds: it forwards the Tool
// interface and exposes the layer beneath for ToolAs.
type deco struct{ inner Tool }

func (d deco) ToolName() string { return d.inner.ToolName() }
func (d deco) isTool()          {}

// Unwrap implements Unwrapper.
func (d deco) Unwrap() Tool { return d.inner }

type approvalTool struct {
	deco
	needs func(ctx context.Context, rc *RunContext, argsJSON, callID string) (bool, error)
}

func (t approvalTool) NeedsToolApproval(ctx context.Context, rc *RunContext, argsJSON, callID string) (bool, error) {
	return t.needs(ctx, rc, argsJSON, callID)
}

// WithApproval makes a tool pause the run for human approval. The predicate
// receives the specific call, so it can decide per invocation.
func WithApproval(t Tool, needs func(ctx context.Context, rc *RunContext, argsJSON, callID string) (bool, error)) Tool {
	return approvalTool{deco{t}, needs}
}

// WithApprovalAlways makes every call to a tool require approval.
func WithApprovalAlways(t Tool) Tool {
	return WithApproval(t, func(context.Context, *RunContext, string, string) (bool, error) {
		return true, nil
	})
}

type timeoutTool struct {
	deco
	d time.Duration
}

func (t timeoutTool) ToolTimeout() time.Duration { return t.d }

// WithTimeout bounds a single invocation of a tool.
func WithTimeout(t Tool, d time.Duration) Tool { return timeoutTool{deco{t}, d} }

type guardedTool struct {
	deco
	guardrails []Guardrail
}

func (t guardedTool) ToolGuardrails() []Guardrail { return t.guardrails }

// WithGuardrails attaches guardrails to a tool's own calls. They ADD to any the
// wrapped tool already declares, rather than replacing them — a wrapper that
// silently dropped an inner tool's safety checks would be a trap.
func WithGuardrails(t Tool, g ...Guardrail) Tool {
	if inner, ok := ToolAs[GuardedTool](t); ok {
		g = append(append([]Guardrail(nil), inner.ToolGuardrails()...), g...)
	}
	return guardedTool{deco{t}, g}
}

type sequentialTool struct{ deco }

func (sequentialTool) RunsSequentially() bool { return true }

// WithSequential marks a tool that must not run concurrently with others in the
// same turn.
func WithSequential(t Tool) Tool { return sequentialTool{deco{t}} }

type enabledTool struct {
	deco
	enabled func(ctx context.Context, rc *RunContext, agent *Agent) (bool, error)
}

func (t enabledTool) IsToolEnabled(ctx context.Context, rc *RunContext, agent *Agent) (bool, error) {
	return t.enabled(ctx, rc, agent)
}

// WithEnabled hides a tool from the model when the predicate says so.
func WithEnabled(t Tool, enabled func(ctx context.Context, rc *RunContext, agent *Agent) (bool, error)) Tool {
	return enabledTool{deco{t}, enabled}
}

type failureHandlingTool struct {
	deco
	handle func(ctx context.Context, tc *ToolContext, err error) string
}

func (t failureHandlingTool) HandleToolFailure(ctx context.Context, tc *ToolContext, err error) string {
	return t.handle(ctx, tc, err)
}

// WithFailureHandler feeds a tool's errors back to the model as output instead
// of aborting the run.
func WithFailureHandler(t Tool, handle func(ctx context.Context, tc *ToolContext, err error) string) Tool {
	return failureHandlingTool{deco{t}, handle}
}
