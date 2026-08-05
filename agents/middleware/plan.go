package middleware

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/zzir/agents-go/agents"
)

// PlanToolName is the tool a Plan-mode agent submits its plan through. Hosts
// use it to recognize the pause: an approval interruption for this tool IS
// the plan review, and the plan text is in the call's arguments.
const PlanToolName = "submit_plan"

// NOTE for hosts: "is this run past its plan phase" has exactly one durable
// answer — the record your PlanPhase.OnUnlock hook wrote. Do not infer it
// from anything else: a REJECTED submit_plan persists as the same
// function_call shape, the executed call's output text can be rewritten by a
// tool-output guardrail, and an approval record can outlive an execution
// that failed (argument validation) — every one of those proxies has
// produced a wrong unlock.

// DefaultReadOnlyTools are the tool names Plan leaves usable while planning.
// Read-only-ness is a NAME CONVENTION, not a tool capability: tools carry no
// side-effect marker, and a list the caller can see and edit beats an
// interface nobody remembers to implement. todo_write is here so stacking
// Todo with Plan works in either order — maintaining the list touches nothing
// outside the run.
var DefaultReadOnlyTools = []string{
	"read_file", "list_files", "read_skill_file", "brave_search", "task_status",
	TodoToolName,
}

// DefaultPlanInstructions is the planning preamble. It tells the model what
// phase it is in, what it can touch, and how to leave the phase — the three
// things a hidden toolset cannot say for itself.
const DefaultPlanInstructions = `You are in PLAN MODE. Before making any changes:
1. Explore with the available read-only tools and understand the task.
2. Write a concrete plan: what you will change, where, and how you will verify it.
3. Submit the plan with the submit_plan tool and wait for approval.
Do not attempt any modification while planning — the tools for that are
disabled and will only become available after your plan is approved. If your
plan is rejected, revise it using the feedback and submit again.`

// Plan puts a run into plan mode: the agent explores with read-only tools,
// submits a plan through submit_plan (which pauses for approval, like any
// approval-gated tool), and only an approved plan unlocks the rest of the
// toolset — in the SAME run, which then continues into execution. A rejection
// feeds its message back and the model revises; the write tools stay hidden.
//
// Gating hides tools rather than failing them: while planning, non-read-only
// tools are absent from the model's toolset (direct tools via their enabled
// hook, MCP tools by filtering each turn's listing), so the model cannot even
// attempt a write. The preamble tells it why.
//
// The middleware rewrites the ENTRY agent only. A handoff target keeps its
// own toolset — same scoping as every instruction-injecting middleware.
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

// PlanPhase is one run's plan/execute switch, shared by every gate that
// Apply installed on the agent. The approved submit_plan flips it; a host
// that REBUILDS the agent to resume a run whose plan phase already ended —
// durable HITL, where the paused state is deserialized against a fresh
// registry — calls Unlock so the rebuilt run starts in the executing phase
// instead of demanding a second plan.
//
// What such a host must persist is the UNLOCK itself (via OnUnlock), not the
// approval: an approved submit_plan can still fail to execute (argument
// validation, say), and its recorded approval would then unlock a later
// resume whose new plan was never accepted.
type PlanPhase struct {
	executing atomic.Bool
	mu        sync.Mutex
	onUnlock  func() error
}

// OnUnlock registers fn to run at the FIRST unlock — the moment the approved
// submit_plan actually executes. Hosts persist their durable "plan phase
// over" mark here. The hook is a PRECONDITION, not a notification: its error
// fails the unlock and the phase stays planning, so the run can never be
// executing ahead of its durable record (a mark that only followed the flip
// left exactly that gap — writes fail).
func (p *PlanPhase) OnUnlock(fn func() error) {
	p.mu.Lock()
	p.onUnlock = fn
	p.mu.Unlock()
}

// Unlock moves the run into the executing phase: gated tools become visible
// and submit_plan disappears. The first transition runs the OnUnlock hook
// first and keeps the phase locked if it fails — the caller (the submit_plan
// tool) surfaces that as a tool error, the model resubmits, and the human
// re-approves; brief friction on a failed write beats a run whose durable
// state disagrees with its behavior.
func (p *PlanPhase) Unlock() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.executing.Load() {
		return nil
	}
	if p.onUnlock != nil {
		if err := p.onUnlock(); err != nil {
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

// Apply returns a clone of agent rewritten for plan mode, plus the phase
// switch the gates share. Run uses it per run; it is exported for hosts that
// must rewrite at agent-BUILD time instead — a host that resumes runs from a
// serialized state rebuilds the agent from a registry, and a rebuild without
// the plan tools would fail the approved submit_plan with "tool not found".
func (p Plan) Apply(agent *agents.Agent) (*agents.Agent, *PlanPhase) {
	names := p.ReadOnlyTools
	if names == nil {
		names = DefaultReadOnlyTools
	}
	readOnly := make(map[string]bool, len(names))
	for _, n := range names {
		readOnly[n] = true
	}

	// The phase flag every gate shares. Atomic because tools may run
	// concurrently within a turn.
	phase := &PlanPhase{}

	out := agent.Clone()
	tools := make([]*agents.Tool, 0, len(out.Tools)+1)
	for _, t := range out.Tools {
		if readOnly[t.Name] {
			tools = append(tools, t)
			continue
		}
		// The gate COMPOSES with the tool's own enabled hook rather than
		// shadowing it: a bare phase gate would make every gated tool
		// unconditionally visible after unlock — including tools the host
		// disabled for permissions or run context. Capturing IsEnabled before
		// overwriting it on the copy is what keeps the tool's own answer.
		gated := *t
		inner := t.IsEnabled
		gated.IsEnabled = func(ctx context.Context, rc *agents.RunContext, agent *agents.Agent) (bool, error) {
			if !phase.Executing() {
				return false, nil
			}
			if inner != nil {
				return inner(ctx, rc, agent)
			}
			return true, nil
		}
		tools = append(tools, &gated)
	}
	submit := agents.NewTool(PlanToolName,
		"Submit your plan for approval. Execution tools unlock only after the plan is approved.",
		func(context.Context, *agents.ToolContext, planArgs) (string, error) {
			// A failed unlock (the host could not persist its durable mark)
			// keeps the phase locked; the error goes back to the model, which
			// resubmits, and the human re-approves.
			if err := phase.Unlock(); err != nil {
				return "", err
			}
			return "Plan approved. Proceed with the implementation; the full toolset is now available.", nil
		})
	// Approval-gated always: the pause IS the review. Hidden again once
	// executing — a second submission would be noise, not a phase change.
	submit.NeedsApproval = true
	submit.IsEnabled = func(context.Context, *agents.RunContext, *agents.Agent) (bool, error) {
		return !phase.Executing(), nil
	}
	tools = append(tools, submit)
	out.Tools = tools

	// Handoffs are gated too: a handoff target keeps its own full toolset, so
	// an ungated transfer would be a side door out of plan mode. IsEnabled is
	// filtered per turn, so approval flips them on mid-run like every gate.
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

	// MCP tools are listed fresh each turn, so a filtering wrapper gates them
	// dynamically: while planning only read-only names survive the listing.
	if len(out.MCPServers) > 0 {
		wrapped := make([]agents.MCPServer, 0, len(out.MCPServers))
		for _, s := range out.MCPServers {
			wrapped = append(wrapped, planMCP{inner: s, phase: phase, readOnly: readOnly})
		}
		out.MCPServers = wrapped
	}

	out.Instructions = agents.WrapInstructions(out.Instructions,
		strings.TrimSpace(firstNonEmpty(p.Instructions, DefaultPlanInstructions)), "")
	return out, phase
}

// planMCP filters an MCP server's per-turn tool listing while planning.
type planMCP struct {
	inner    agents.MCPServer
	phase    *PlanPhase
	readOnly map[string]bool
}

func (m planMCP) Name() string { return m.inner.Name() }
func (m planMCP) Close() error { return m.inner.Close() }

func (m planMCP) ListTools(ctx context.Context, rc *agents.RunContext, agent *agents.Agent) ([]*agents.Tool, error) {
	tools, err := m.inner.ListTools(ctx, rc, agent)
	if err != nil || m.phase.Executing() {
		return tools, err
	}
	// A fresh slice, never tools[:0]: the inner server may hand out a cached
	// slice, and filtering in place would corrupt it for every later turn.
	kept := make([]*agents.Tool, 0, len(tools))
	for _, t := range tools {
		if m.readOnly[t.Name] {
			kept = append(kept, t)
		}
	}
	return kept, nil
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
