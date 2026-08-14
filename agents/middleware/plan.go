package middleware

import (
	"context"
	"fmt"
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
//
// The same record is what re-arms a RESUMED run: Apply returns a fresh,
// locked PlanPhase, so rebuilding the agent for agents.RunStateFromJSON
// resets the phase, and a run that paused mid-execution would come back
// without its write tools. Consult your unlock record (or the copy a pause
// carried in agents.RunState.Extra) and call Unlock before ResumeRun — see
// docs/human_in_the_loop.md "Rebuilding transformed agents".

// DefaultReadOnlyTools are extra tool names Plan leaves usable while planning,
// on top of every tool that declares `Tool.ReadOnly`. The list covers what a
// caller does not own: a tool from another package, or an MCP server that
// ships no readOnlyHint. todo_write is here so stacking Todo with Plan works
// in either order — maintaining the list touches nothing outside the run.
var DefaultReadOnlyTools = []string{
	"read_file", "list_files", "read_skill_file", "brave_search", "task_status",
	TodoToolName,
}

// DefaultPlanInstructions is the planning preamble. It tells the model what
// phase it is in, what it can touch, and how to leave the phase — the three
// things a hidden toolset cannot say for itself.
const DefaultPlanInstructions = `You are in PLAN MODE. Before making any changes:
1. Understand the task, exploring with the read-only tools in your toolset.
   Work from the tools you can see — this session may have no filesystem or
   shell access at all, and a tool that is not listed does not exist here.
2. Write a concrete plan: what you will change, where, and how you will verify it.
3. Submit the plan with the submit_plan tool and wait for approval.
Do not attempt any modification while planning — those tools are listed but
disabled, and answer with a refusal until your plan is approved. If your plan
is rejected, revise it using the feedback and submit again.`

// Plan puts a run into plan mode: the agent explores with read-only tools,
// submits a plan through submit_plan (which pauses for approval, like any
// approval-gated tool), and only an approved plan unlocks the rest of the
// toolset — in the SAME run, which then continues into execution. A rejection
// feeds its message back and the model revises; the write tools stay locked.
//
// Gating DENIES rather than hides: a gated tool stays in the model's toolset
// and answers a call while planning with a refusal naming what to do instead.
// A model that cannot see a tool it expects reaches for it anyway and gets
// "tool not found", which teaches it nothing; a refusal teaches it the phase.
// A FIRST-PARTY tool declares itself read-only through Tool.ReadOnly; an MCP
// tool's ReadOnly is the server's own readOnlyHint, which the gate does NOT
// trust — an MCP tool is admitted only when named in ReadOnlyTools.
//
// The refusal outranks approval: a gated call while planning raises NO human
// approval — not the tool's own and not the agent's ApproveTools listing,
// which Apply translates into per-tool predicates (and clears) so the phase
// can suppress it. Read-only tools keep their approval in both phases.
//
// The middleware rewrites the ENTRY agent only. A handoff target keeps its
// own toolset — same scoping as every instruction-injecting middleware.
//
// Apply is safe to call unconditionally: an already-unlocked phase gates
// nothing, offers no submit_plan and adds no preamble. Whether THIS run plans
// is the returned PlanPhase's answer, not a build-time one — which is what
// lets plan mode be a property of the session (or the person's request)
// rather than of the agent.
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
	// The agent-level ApproveTools list is consulted by the runner BEFORE a
	// tool runs, so it would pause a human over a call the planning gate is
	// about to refuse. Apply therefore translates the list into each tool's own
	// predicate — where the gate can suppress it while planning — and clears
	// it; after unlock the translated predicates answer exactly as the list did.
	listed := approvalListMatcher(out.ApproveTools)
	out.ApproveTools = nil
	tools := make([]*agents.Tool, 0, len(out.Tools)+1)
	for _, t := range out.Tools {
		if t.ReadOnly || readOnly[t.Name] {
			tools = append(tools, keepListedApproval(t, listed(t.Name)))
			continue
		}
		tools = append(tools, gateTool(t, phase, listed(t.Name)))
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

	// MCP tools are listed fresh each turn, so a wrapper gates them per listing
	// — the same refusal as a direct tool, applied to a set Plan cannot see at
	// build time. The wrapper also carries the translated ApproveTools listing:
	// the list was cleared above, and an MCP tool it named must keep requiring
	// approval once executing.
	if len(out.MCPServers) > 0 {
		wrapped := make([]agents.MCPServer, 0, len(out.MCPServers))
		for _, s := range out.MCPServers {
			wrapped = append(wrapped, planMCP{inner: s, phase: phase, readOnly: readOnly, listed: listed})
		}
		out.MCPServers = wrapped
	}

	// The preamble is emitted only while the phase is LOCKED. Instructions are
	// computed per run, so an agent that starts (or is rebuilt) already
	// unlocked carries none of it — which is what lets a host apply Plan
	// unconditionally and decide the phase separately.
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

// gateTool returns a copy of t that refuses to run while the plan phase is
// still planning. The refusal is a normal tool OUTPUT, not an error: an error
// without a FailureErrorFunction aborts the run, and a phase decision is not a
// failure — the model is meant to read it and submit a plan.
//
// The refusal fires BEFORE the approval gate: while planning the copy answers
// "no approval needed", because pausing a human over a call the phase refuses
// anyway wastes the interruption on a no-op. Once executing, the tool's own
// predicate answers, then the agent-level listing (`listed`) Apply translated
// out of ApproveTools — see spec §"Plan gates by DENYING".
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

// keepListedApproval returns t, or — when the agent's ApproveTools named it —
// a copy whose own predicate now also enforces the listing, since Apply
// cleared the list itself. Read-only tools keep their approval in BOTH phases.
func keepListedApproval(t *agents.Tool, listed bool) *agents.Tool {
	if !listed {
		return t
	}
	kept := *t
	innerFunc := t.NeedsApprovalFunc
	kept.NeedsApprovalFunc = func(ctx context.Context, rc *agents.RunContext, argsJSON, callID string) (bool, error) {
		if innerFunc != nil {
			// Invoked for its error (and any per-call effects), exactly as the
			// runner would have; a non-error answer is superseded by the listing.
			if _, err := innerFunc(ctx, rc, argsJSON, callID); err != nil {
				return false, err
			}
		}
		return true, nil
	}
	return &kept
}

// approvalListMatcher is agentApprovesToolName's semantics as a predicate:
// exact name, or "*" for every tool.
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

// planMCP gates an MCP server's per-turn tool listing while planning, and
// carries the translated ApproveTools listing in both phases (the agent-level
// list was cleared by Apply, so MCP tools it named enforce it themselves).
type planMCP struct {
	inner    agents.MCPServer
	phase    *PlanPhase
	readOnly map[string]bool
	listed   func(string) bool
}

func (m planMCP) Name() string { return m.inner.Name() }
func (m planMCP) Close() error { return m.inner.Close() }

func (m planMCP) ListTools(ctx context.Context, rc *agents.RunContext, agent *agents.Agent) ([]*agents.Tool, error) {
	tools, err := m.inner.ListTools(ctx, rc, agent)
	if err != nil {
		return tools, err
	}
	// A fresh slice, never tools[:0]: the inner server may hand out a cached
	// slice, and rewriting in place would corrupt it for every later turn.
	// The gates check the phase per CALL, so one wrapping serves both phases.
	out := make([]*agents.Tool, 0, len(tools))
	for _, t := range tools {
		// By NAME only — never the tool's own ReadOnly. On an MCP tool that flag
		// is the server's readOnlyHint, a claim the external server makes about
		// itself; trusting it would let a server mark a write tool "read-only"
		// and run it while the person believes nothing can change. Plan mode's
		// guarantee cannot rest on an outside declaration, so an MCP tool is
		// admitted only when the CALLER named it in ReadOnlyTools.
		if m.readOnly[t.Name] {
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
