package middleware

import (
	"context"
	"errors"
	"iter"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zzir/agents-go/agents"
)

// recordingModel is a scriptedModel that also records the tool names offered
// on each call — plan mode's whole point is what the model can SEE per phase.
type recordingModel struct {
	scriptedModel
	toolsPerCall [][]string
}

func (m *recordingModel) record(req agents.ModelRequest) {
	names := make([]string, 0, len(req.Tools)+len(req.Handoffs))
	for _, t := range req.Tools {
		names = append(names, t.Name)
	}
	// Handoffs surface to the model as transfer tools; record them alongside
	// so the tests can pin their phase gating too.
	for _, h := range req.Handoffs {
		names = append(names, h.ToolName)
	}
	m.toolsPerCall = append(m.toolsPerCall, names)
}

func (m *recordingModel) Respond(ctx context.Context, req agents.ModelRequest) (*agents.ModelResponse, error) {
	m.record(req)
	return m.scriptedModel.Respond(ctx, req)
}

func (m *recordingModel) StreamResponse(ctx context.Context, req agents.ModelRequest) iter.Seq2[*agents.ResponseStreamEvent, error] {
	m.record(req)
	return m.scriptedModel.StreamResponse(ctx, req)
}

func toolCallArgs(t *testing.T, name, callID, args string) agents.OutputItem {
	t.Helper()
	return outputItem(t, `{"type":"function_call","id":"fc_1","call_id":`+quote(callID)+
		`,"name":`+quote(name)+`,"arguments":`+quote(args)+`,"status":"completed"}`)
}

// noopTool is a recording function tool with no behavior.
func noopTool(name string, calls *atomic.Int32) *agents.Tool {
	return agents.NewTool(name, "test tool",
		func(context.Context, *agents.ToolContext, struct{}) (string, error) {
			if calls != nil {
				calls.Add(1)
			}
			return "ok", nil
		})
}

// The full arc: planning locks the write tool, submit_plan pauses for
// approval, approval unlocks the toolset and the SAME run continues to done.
func TestPlan_ApproveUnlocksExecutionInTheSameRun(t *testing.T) {
	var writes atomic.Int32
	model := &recordingModel{scriptedModel: scriptedModel{responses: []*agents.ModelResponse{
		resp(toolCallArgs(t, PlanToolName, "c1", `{"plan":"change the thing"}`)),
		resp(toolCallArgs(t, "write_file", "c2", `{}`)),
		resp(message(t, "done")),
	}}}
	// A write tool the HOST already disabled: the plan gate must compose with
	// its hook, not shadow it — unlock must not resurrect it.
	lockedWrite := noopTool("locked_write", nil)
	lockedWrite.IsEnabled = func(context.Context, *agents.RunContext, *agents.Agent) (bool, error) { return false, nil }
	agent := &agents.Agent{
		Name:      "a",
		ModelImpl: model,
		Tools:     []*agents.Tool{noopTool("read_file", nil), noopTool("write_file", &writes), lockedWrite},
		Handoffs:  []agents.Handoff{agents.HandoffTo(&agents.Agent{Name: "other"})},
	}
	opts := agents.RunOptions{Middlewares: []agents.RunMiddleware{Plan{}}}

	res, err := agents.RunSync(context.Background(), agent, "go", opts)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Interruptions) != 1 || res.Interruptions[0].ToolName != PlanToolName {
		t.Fatalf("want one %s interruption, got %+v", PlanToolName, res.Interruptions)
	}
	// Phase one saw the read tool, submit_plan AND the write tool — gated tools
	// stay visible and refuse when called. No handoff, though: a handoff
	// target's full toolset would be a side door out of plan mode.
	if got := model.toolsPerCall[0]; !slices.Contains(got, "read_file") ||
		!slices.Contains(got, PlanToolName) || !slices.Contains(got, "write_file") ||
		slices.Contains(got, "transfer_to_other") {
		t.Fatalf("planning toolset = %v", got)
	}
	if writes.Load() != 0 {
		t.Fatal("a write ran while planning")
	}

	res.State.Approve(res.Interruptions[0], false)
	final, err := agents.ResumeRunSync(context.Background(), res.State, agents.RunOptions{})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if final.FinalOutputString() != "done" {
		t.Fatalf("final = %q", final.FinalOutputString())
	}
	if writes.Load() != 1 {
		t.Fatalf("write tool ran %d times, want 1", writes.Load())
	}
	// Phase two saw the write tool and the handoff, and no submit_plan — and
	// the host-disabled write tool stayed hidden: the unlock opens the plan
	// gate, it does not override the tool's own enabled hook.
	last := model.toolsPerCall[len(model.toolsPerCall)-1]
	if !slices.Contains(last, "write_file") || slices.Contains(last, PlanToolName) ||
		!slices.Contains(last, "transfer_to_other") {
		t.Fatalf("execution toolset = %v", last)
	}
	if slices.Contains(last, "locked_write") {
		t.Fatalf("unlock resurrected a host-disabled tool: %v", last)
	}
}

// A rejection is feedback, not an unlock: the model plans again with the
// write tools still refusing.
func TestPlan_RejectKeepsPlanning(t *testing.T) {
	var writes atomic.Int32
	model := &recordingModel{scriptedModel: scriptedModel{responses: []*agents.ModelResponse{
		resp(toolCallArgs(t, PlanToolName, "c1", `{"plan":"v1"}`)),
		resp(message(t, "let me rethink")),
	}}}
	agent := &agents.Agent{
		Name:      "a",
		ModelImpl: model,
		Tools:     []*agents.Tool{noopTool("write_file", &writes)},
	}
	res, err := agents.RunSync(context.Background(), agent, "go",
		agents.RunOptions{Middlewares: []agents.RunMiddleware{Plan{}}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	res.State.Reject(res.Interruptions[0], false, "too vague — name the files")
	if _, err := agents.ResumeRunSync(context.Background(), res.State, agents.RunOptions{}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	last := model.toolsPerCall[len(model.toolsPerCall)-1]
	if !slices.Contains(last, PlanToolName) {
		t.Fatalf("submit_plan gone after a rejection: %v", last)
	}
	if writes.Load() != 0 {
		t.Fatal("a write ran after a rejection")
	}
}

// Calling a gated tool while planning is answered, not fatal: the tool does
// not run, the model gets a refusal naming submit_plan, and the run goes on.
// Hiding the tool instead produced "tool not found", which a model cannot tell
// from a tool this session never had.
func TestPlan_GatedToolRefusesInsteadOfFailing(t *testing.T) {
	var writes atomic.Int32
	model := &recordingModel{scriptedModel: scriptedModel{responses: []*agents.ModelResponse{
		resp(toolCallArgs(t, "write_file", "c1", `{}`)),
		resp(message(t, "right, planning first")),
	}}}
	agent := &agents.Agent{
		Name:      "a",
		ModelImpl: model,
		Tools:     []*agents.Tool{noopTool("write_file", &writes)},
	}
	res, err := agents.RunSync(context.Background(), agent, "go",
		agents.RunOptions{Middlewares: []agents.RunMiddleware{Plan{}}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.FinalOutputString() != "right, planning first" {
		t.Fatalf("final = %q — the refusal should not end the run", res.FinalOutputString())
	}
	if writes.Load() != 0 {
		t.Fatal("a gated tool ran while planning")
	}
	var refusal string
	for _, it := range res.NewItems {
		if it.Kind == agents.ItemToolCallOutput {
			refusal, _ = it.Output.(string)
		}
	}
	if !strings.Contains(refusal, PlanToolName) || !strings.Contains(refusal, "write_file") {
		t.Fatalf("refusal = %q, want it to name the tool and submit_plan", refusal)
	}
}

// fakeMCP lists a fixed set of tools.
type fakeMCP struct{ tools []*agents.Tool }

func (f fakeMCP) Name() string { return "fake" }
func (f fakeMCP) Close() error { return nil }
func (f fakeMCP) ListTools(context.Context, *agents.RunContext, *agents.Agent) ([]*agents.Tool, error) {
	return slices.Clone(f.tools), nil
}

// MCP tools are listed fresh each turn, so the gate is applied per listing:
// every tool stays listed, and while planning only the ones the CALLER named in
// ReadOnlyTools are usable. A server's own readOnlyHint does NOT admit a tool —
// it is the external server's claim about itself, and plan mode's guarantee
// cannot rest on it: a write tool marked "read-only" by a hostile server is
// still gated.
func TestPlan_MCPListingIsPhaseGated(t *testing.T) {
	var hintedWrites atomic.Int32
	hinted := noopTool("mcp__search", &hintedWrites)
	hinted.ReadOnly = true // the server's readOnlyHint — not to be trusted
	var writes atomic.Int32
	inner := fakeMCP{tools: []*agents.Tool{noopTool("read_file", nil), hinted, noopTool("mcp__write", &writes)}}
	phase := &PlanPhase{}
	m := planMCP{inner: inner, phase: phase, readOnly: map[string]bool{"read_file": true},
		listed: approvalListMatcher(nil)}

	planning, err := m.ListTools(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := toolNames(planning); len(got) != 3 {
		t.Fatalf("planning listing = %v, want every tool listed", got)
	}
	// Only the name-listed read_file is admitted untouched; BOTH the
	// hint-only tool and the plain write tool are gated.
	if planning[0] != inner.tools[0] {
		t.Fatal("the name-listed tool was wrapped")
	}
	for _, i := range []int{1, 2} {
		out, err := planning[i].OnInvoke(context.Background(), nil, "{}")
		if err != nil {
			t.Fatalf("gated invoke %d: %v", i, err)
		}
		if text, _ := out.Content[0].(agents.ToolOutputText); !strings.Contains(text.Text, PlanToolName) {
			t.Fatalf("tool %d refusal = %+v, want it to name %s", i, out.Content, PlanToolName)
		}
	}
	if hintedWrites.Load() != 0 {
		t.Fatal("a tool the server merely HINTED was read-only ran while planning")
	}
	if writes.Load() != 0 {
		t.Fatal("a gated MCP tool ran while planning")
	}

	if err := phase.Unlock(); err != nil {
		t.Fatal(err)
	}
	after, err := m.ListTools(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := after[2].OnInvoke(context.Background(), nil, "{}"); err != nil {
		t.Fatalf("invoke after unlock: %v", err)
	}
	if writes.Load() != 1 {
		t.Fatal("the tool did not run after the unlock")
	}
}

func toolNames(tools []*agents.Tool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name)
	}
	return out
}

// The OnUnlock hook is the unlock's PRECONDITION: it fires exactly once, at
// the first successful transition, and its error keeps the phase locked — so
// a run can never be executing ahead of the host's durable record.
func TestPlanPhase_UnlockGatedByHook(t *testing.T) {
	phase := &PlanPhase{}
	fired := 0
	fail := true
	phase.OnUnlock(func() error {
		fired++
		if fail {
			return errors.New("db down")
		}
		return nil
	})

	if err := phase.Unlock(); err == nil {
		t.Fatal("a failed hook must fail the unlock")
	}
	if phase.Executing() {
		t.Fatal("the phase must stay locked when the hook fails")
	}

	fail = false
	if err := phase.Unlock(); err != nil {
		t.Fatalf("retry after hook recovery: %v", err)
	}
	if !phase.Executing() {
		t.Fatal("phase must be executing after a successful unlock")
	}
	if err := phase.Unlock(); err != nil {
		t.Fatalf("idempotent unlock: %v", err)
	}
	if fired != 2 {
		t.Fatalf("hook fired %d times, want 2 (failed attempt + success; never after)", fired)
	}
}

// The refusal outranks approval: a gated call while planning pauses nobody —
// neither the tool's own NeedsApproval nor the agent's ApproveTools listing —
// because the phase refuses the call whatever a human would answer.
func TestPlan_GatedApprovalToolsRaiseNoApprovalWhilePlanning(t *testing.T) {
	var writes, deploys atomic.Int32
	model := &recordingModel{scriptedModel: scriptedModel{responses: []*agents.ModelResponse{
		resp(toolCallArgs(t, "write_file", "c1", `{}`), toolCallArgs(t, "deploy", "c2", `{}`)),
		resp(message(t, "planning")),
	}}}
	write := noopTool("write_file", &writes)
	write.NeedsApproval = true
	agent := &agents.Agent{
		Name:         "a",
		ModelImpl:    model,
		Tools:        []*agents.Tool{noopTool("read_file", nil), write, noopTool("deploy", &deploys)},
		ApproveTools: []string{"deploy"},
	}
	res, err := agents.RunSync(context.Background(), agent, "go",
		agents.RunOptions{Middlewares: []agents.RunMiddleware{Plan{}}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Interruptions) != 0 {
		t.Fatalf("planning raised %d approval interruption(s) for gated tools, want 0: %+v",
			len(res.Interruptions), res.Interruptions)
	}
	if writes.Load() != 0 || deploys.Load() != 0 {
		t.Fatal("a gated tool ran while planning")
	}
	if res.FinalOutputString() != "planning" {
		t.Fatalf("final = %q — the refusals should feed back, not pause", res.FinalOutputString())
	}
}

// After the unlock, the suppressed approvals come back: the tool's own
// NeedsApproval and the ApproveTools listing (translated into the tool by
// Apply) both pause exactly as they would without plan mode.
func TestPlan_ApprovalSurvivesTheUnlock(t *testing.T) {
	var writes, deploys atomic.Int32
	model := &recordingModel{scriptedModel: scriptedModel{responses: []*agents.ModelResponse{
		resp(toolCallArgs(t, PlanToolName, "c1", `{"plan":"p"}`)),
		resp(toolCallArgs(t, "write_file", "c2", `{}`), toolCallArgs(t, "deploy", "c3", `{}`)),
		resp(message(t, "done")),
	}}}
	write := noopTool("write_file", &writes)
	write.NeedsApproval = true
	agent := &agents.Agent{
		Name:         "a",
		ModelImpl:    model,
		Tools:        []*agents.Tool{write, noopTool("deploy", &deploys)},
		ApproveTools: []string{"deploy"},
	}
	res, err := agents.RunSync(context.Background(), agent, "go",
		agents.RunOptions{Middlewares: []agents.RunMiddleware{Plan{}}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Interruptions) != 1 || res.Interruptions[0].ToolName != PlanToolName {
		t.Fatalf("want the submit_plan pause, got %+v", res.Interruptions)
	}
	res.State.Approve(res.Interruptions[0], false)
	res2, err := agents.ResumeRunSync(context.Background(), res.State, agents.RunOptions{})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	names := make([]string, 0, 2)
	for _, it := range res2.Interruptions {
		names = append(names, it.ToolName)
	}
	if len(res2.Interruptions) != 2 {
		t.Fatalf("executing phase: want approvals for write_file (own) and deploy (listed), got %v", names)
	}
	for _, it := range res2.Interruptions {
		res2.State.Approve(it, false)
	}
	final, err := agents.ResumeRunSync(context.Background(), res2.State, agents.RunOptions{})
	if err != nil {
		t.Fatalf("second resume: %v", err)
	}
	if final.FinalOutputString() != "done" {
		t.Fatalf("final = %q", final.FinalOutputString())
	}
	if writes.Load() != 1 || deploys.Load() != 1 {
		t.Fatalf("approved tools ran %d/%d times, want 1/1", writes.Load(), deploys.Load())
	}
}

// A READ-ONLY tool the ApproveTools listing names keeps its approval in BOTH
// phases: the phase never suppresses approval on a tool it is not refusing.
func TestPlan_ListedReadOnlyToolStillPausesWhilePlanning(t *testing.T) {
	var reads atomic.Int32
	model := &recordingModel{scriptedModel: scriptedModel{responses: []*agents.ModelResponse{
		resp(toolCallArgs(t, "read_file", "c1", `{}`)),
		resp(message(t, "read it")),
	}}}
	agent := &agents.Agent{
		Name:         "a",
		ModelImpl:    model,
		Tools:        []*agents.Tool{noopTool("read_file", &reads)},
		ApproveTools: []string{"read_file"},
	}
	res, err := agents.RunSync(context.Background(), agent, "go",
		agents.RunOptions{Middlewares: []agents.RunMiddleware{Plan{}}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Interruptions) != 1 || res.Interruptions[0].ToolName != "read_file" {
		t.Fatalf("want the listed read-only tool to pause while planning, got %+v", res.Interruptions)
	}
	res.State.Approve(res.Interruptions[0], false)
	if _, err := agents.ResumeRunSync(context.Background(), res.State, agents.RunOptions{}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if reads.Load() != 1 {
		t.Fatal("the approved read did not run")
	}
}
