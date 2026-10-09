package middleware

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/zzir/agents-go/agents"
)

// PlanToolName is the tool a Plan-mode agent submits its plan through; an
// approval interruption for it IS the plan review, the plan in its arguments.
const PlanToolName = "submit_plan"

// DefaultReadOnlyTools are the tool names Plan admits while planning when
// ReadOnlyTools is nil, beside every tool that declares Tool.ReadOnly.
var DefaultReadOnlyTools = []string{"read_file", "list_files", "task_status"}

// DefaultPlanInstructions is the planning preamble.
const DefaultPlanInstructions = `You are in PLAN MODE. Before making any changes:
1. Understand the task, exploring with the read-only tools in your toolset.
   Work from the tools you can see — this session may have no filesystem or
   shell access at all, and a tool that is not listed does not exist here.
2. Write a concrete plan: what you will change, where, and how you will verify it.
3. Submit the plan with the submit_plan tool and wait for approval.
Do not attempt any modification while planning — those tools are listed but
disabled, and answer with a refusal until your plan is approved. If your plan
is rejected, revise it using the feedback and submit again.`

// ReadOnlySet is the tool names plan mode admits while planning; Admits is the
// one predicate the plan gate and a host's approval share — see spec §2.12.
type ReadOnlySet map[string]bool

// Admits reports whether t is usable while planning: by its ReadOnly flag or a
// listed name, by a listed name only when fromMCP — see decisions §5.53.
func (s ReadOnlySet) Admits(t *agents.Tool, fromMCP bool) bool {
	return (!fromMCP && t.ReadOnly) || s[t.Name]
}

// ReadOnlySet is the set p plans with: ReadOnlyTools, or DefaultReadOnlyTools when nil.
func (p Plan) ReadOnlySet() ReadOnlySet {
	names := p.ReadOnlyTools
	if names == nil {
		names = DefaultReadOnlyTools
	}
	set := make(ReadOnlySet, len(names))
	for _, n := range names {
		set[n] = true
	}
	return set
}

// Plan puts a run into plan mode: the agent explores with read-only tools and
// submits a plan through submit_plan, whose approval unlocks the rest of the
// toolset in the same run — see spec §2.12.
type Plan struct {
	// ReadOnlyTools are the tool names usable while planning.
	// Nil means DefaultReadOnlyTools; an explicit empty slice means none.
	ReadOnlyTools []string
	// Instructions overrides the planning preamble (empty = DefaultPlanInstructions).
	Instructions string
}

// planArgs is what the model hands submit_plan.
type planArgs struct {
	Plan string `json:"plan" jsonschema:"The full plan, in markdown: intended changes, affected files or systems, and how the result will be verified."`
}

// PlanPhase is one run's plan/execute switch, shared by every gate Apply
// installed; the approved submit_plan or a host's Unlock flips it — see spec §2.12.
type PlanPhase struct {
	executing atomic.Bool
	mu        sync.Mutex
	onUnlock  func(plan string) error
}

// OnUnlock registers fn to run once, at the first unlock, with the approved
// plan's text (empty from Unlock); its error keeps the phase planning (spec §2.12).
func (p *PlanPhase) OnUnlock(fn func(plan string) error) {
	p.mu.Lock()
	p.onUnlock = fn
	p.mu.Unlock()
}

// Unlock moves the run into the executing phase; the first transition runs
// OnUnlock first and stays locked if it fails.
func (p *PlanPhase) Unlock() error { return p.unlock("") }

// unlock is Unlock carrying the plan an approved submit_plan was called with;
// submit_plan reports its failure as a tool error.
func (p *PlanPhase) unlock(plan string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.executing.Load() {
		return nil
	}
	if p.onUnlock != nil {
		if err := p.onUnlock(plan); err != nil {
			return err
		}
	}
	p.executing.Store(true)
	return nil
}

// Executing reports the current phase.
func (p *PlanPhase) Executing() bool { return p.executing.Load() }

// Run implements agents.RunMiddleware.
func (p Plan) Run(ctx context.Context, next agents.RunFunc, in agents.RunInput) agents.RunStream {
	if in.Agent == nil {
		return next(ctx, in)
	}
	in.Agent, _ = p.Apply(in.Agent)
	return next(ctx, in)
}

// Apply returns a clone of agent rewritten for plan mode, plus the phase switch
// the gates share; a durable-resume host calls it at build time (spec §2.12).
func (p Plan) Apply(agent *agents.Agent) (*agents.Agent, *PlanPhase) {
	readOnly := p.ReadOnlySet()
	phase := &PlanPhase{}

	out := agent.Clone()
	// ApproveTools is translated into per-tool predicates the phase can
	// suppress (spec §2.12).
	listed := approvalListMatcher(out.ApproveTools)
	out.ApproveTools = nil
	tools := make([]*agents.Tool, 0, len(out.Tools)+1)
	for _, t := range out.Tools {
		if readOnly.Admits(t, false) {
			tools = append(tools, keepListedApproval(t, listed(t.Name)))
			continue
		}
		tools = append(tools, gateTool(t, phase, listed(t.Name)))
	}
	submit := agents.NewTool(PlanToolName,
		"Submit your plan for approval. Execution tools unlock only after the plan is approved.",
		func(_ context.Context, _ *agents.ToolContext, args planArgs) (string, error) {
			// A failed unlock keeps the phase locked; the model resubmits.
			if err := phase.unlock(args.Plan); err != nil {
				return "", err
			}
			return "Plan approved. Proceed with the implementation; the full toolset is now available.", nil
		})
	// Always approval-gated (the pause IS the review); hidden once executing.
	submit.NeedsApproval = true
	submit.IsEnabled = func(context.Context, *agents.RunContext, *agents.Agent) (bool, error) {
		return !phase.Executing(), nil
	}
	tools = append(tools, submit)
	out.Tools = tools

	// Handoffs are hidden while planning; IsEnabled is filtered per turn — see
	// decisions §5.53.
	if len(out.Handoffs) > 0 {
		hs := make([]agents.Handoff, len(out.Handoffs))
		copy(hs, out.Handoffs)
		for i := range hs {
			inner := hs[i].IsEnabled
			hs[i].IsEnabled = func(ctx context.Context, rc *agents.RunContext, agent *agents.Agent) (bool, error) {
				if !phase.Executing() {
					return false, nil
				}
				if inner != nil {
					return inner(ctx, rc, agent)
				}
				return true, nil
			}
		}
		out.Handoffs = hs
	}

	// MCP tools are listed fresh each turn, so a wrapper gates them per listing.
	if len(out.MCPServers) > 0 {
		wrapped := make([]agents.MCPServer, 0, len(out.MCPServers))
		for _, s := range out.MCPServers {
			wrapped = append(wrapped, planMCP{inner: s, phase: phase, readOnly: readOnly, listed: listed})
		}
		out.MCPServers = wrapped
	}

	// The preamble is emitted only while the phase is locked.
	preamble := strings.TrimSpace(firstNonEmpty(p.Instructions, DefaultPlanInstructions))
	inner := out.Instructions
	out.Instructions = func(ctx context.Context, rc *agents.RunContext, agent *agents.Agent) (string, error) {
		if phase.Executing() {
			if inner == nil {
				return "", nil
			}
			return inner(ctx, rc, agent)
		}
		return agents.WrapInstructions(inner, preamble, "")(ctx, rc, agent)
	}
	return out, phase
}

// gateTool returns a copy of t that answers a call while planning with a
// refusal and needs no approval until executing — spec §2.12.
func gateTool(t *agents.Tool, phase *PlanPhase, listed bool) *agents.Tool {
	gated := *t
	inner := t.OnInvoke
	gated.OnInvoke = func(ctx context.Context, tc *agents.ToolContext, argsJSON string) (agents.ToolResult, error) {
		if !phase.Executing() {
			return agents.TextResult(fmt.Sprintf(
				"%s is disabled while planning. Finish understanding the task with your read-only tools, "+
					"then call %s; every tool unlocks once the plan is approved.", t.Name, PlanToolName)), nil
		}
		if inner == nil {
			return agents.ToolResult{}, fmt.Errorf("tool %q has no OnInvoke", t.Name)
		}
		return inner(ctx, tc, argsJSON)
	}
	if innerFunc, innerBool := t.NeedsApprovalFunc, t.NeedsApproval; innerFunc != nil || innerBool || listed {
		gated.NeedsApprovalFunc = func(ctx context.Context, rc *agents.RunContext, argsJSON, callID string) (bool, error) {
			if !phase.Executing() {
				return false, nil
			}
			if innerFunc != nil {
				need, err := innerFunc(ctx, rc, argsJSON, callID)
				if err != nil || need {
					return need, err
				}
			} else if innerBool {
				return true, nil
			}
			return listed, nil
		}
	}
	return &gated
}

// keepListedApproval returns t, or when ApproveTools named it a copy whose own
// predicate enforces the listing in both phases.
func keepListedApproval(t *agents.Tool, listed bool) *agents.Tool {
	if !listed {
		return t
	}
	kept := *t
	innerFunc := t.NeedsApprovalFunc
	kept.NeedsApprovalFunc = func(ctx context.Context, rc *agents.RunContext, argsJSON, callID string) (bool, error) {
		if innerFunc != nil {
			// Invoked for its error and per-call effects; its answer is
			// superseded by the listing.
			if _, err := innerFunc(ctx, rc, argsJSON, callID); err != nil {
				return false, err
			}
		}
		return true, nil
	}
	return &kept
}

// approvalListMatcher is the ApproveTools listing as a predicate: exact name,
// or "*" for every tool.
func approvalListMatcher(names []string) func(string) bool {
	all := false
	set := make(map[string]bool, len(names))
	for _, n := range names {
		if n == "*" {
			all = true
			continue
		}
		set[n] = true
	}
	return func(name string) bool { return all || set[name] }
}

// planMCP gates an MCP server's per-turn listing while planning and carries
// the translated ApproveTools listing in both phases.
type planMCP struct {
	inner    agents.MCPServer
	phase    *PlanPhase
	readOnly ReadOnlySet
	listed   func(string) bool
}

func (m planMCP) Name() string { return m.inner.Name() }
func (m planMCP) Close() error { return m.inner.Close() }

func (m planMCP) ListTools(ctx context.Context, rc *agents.RunContext, agent *agents.Agent) ([]*agents.Tool, error) {
	tools, err := m.inner.ListTools(ctx, rc, agent)
	if err != nil {
		return tools, err
	}
	// A fresh slice, never tools[:0]: the inner server may hand out a cached one.
	out := make([]*agents.Tool, 0, len(tools))
	for _, t := range tools {
		if m.readOnly.Admits(t, true) {
			out = append(out, keepListedApproval(t, m.listed(t.Name)))
			continue
		}
		out = append(out, gateTool(t, m.phase, m.listed(t.Name)))
	}
	return out, nil
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

var (
	_ agents.RunMiddleware = Plan{}
	_ agents.MCPServer     = planMCP{}
)
