package middleware

import (
	"context"
	"errors"
	"iter"
	"slices"
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
		names = append(names, t.ToolName())
	}
	// Handoffs surface to the model as transfer tools; record them alongside
	// so the tests can pin their phase gating too.
	for _, h := range req.Handoffs {
		names = append(names, h.ToolName)
	}
	m.toolsPerCall = append(m.toolsPerCall, names)
}

func (m *recordingModel) GetResponse(ctx context.Context, req agents.ModelRequest) (*agents.ModelResponse, error) {
	m.record(req)
	return m.scriptedModel.GetResponse(ctx, req)
}

func (m *recordingModel) StreamResponse(ctx context.Context, req agents.ModelRequest) iter.Seq2[*agents.TResponseStreamEvent, error] {
	m.record(req)
	return m.scriptedModel.StreamResponse(ctx, req)
}

func toolCallArgs(t *testing.T, name, callID, args string) agents.TResponseOutputItem {
	t.Helper()
	return outputItem(t, `{"type":"function_call","id":"fc_1","call_id":`+quote(callID)+
		`,"name":`+quote(name)+`,"arguments":`+quote(args)+`,"status":"completed"}`)
}

// noopTool is a recording function tool with no behavior.
func noopTool(name string, calls *atomic.Int32) agents.Tool {
	return agents.NewFunctionTool(name, "test tool",
		func(context.Context, *agents.ToolContext, struct{}) (string, error) {
			if calls != nil {
				calls.Add(1)
			}
			return "ok", nil
		})
}

// The full arc: planning hides the write tool, submit_plan pauses for
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
	lockedWrite := agents.WithEnabled(noopTool("locked_write", nil),
		func(context.Context, *agents.RunContext, *agents.Agent) (bool, error) { return false, nil })
	agent := &agents.Agent{
		Name:      "a",
		ModelImpl: model,
		Tools:     []agents.Tool{noopTool("read_file", nil), noopTool("write_file", &writes), lockedWrite},
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
	// Phase one saw the read tool and submit_plan — never the write tool, and
	// no handoff either (a handoff target's full toolset would be a side door
	// out of plan mode).
	if got := model.toolsPerCall[0]; !slices.Contains(got, "read_file") ||
		!slices.Contains(got, PlanToolName) || slices.Contains(got, "write_file") ||
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
// write tools still hidden.
func TestPlan_RejectKeepsPlanning(t *testing.T) {
	var writes atomic.Int32
	model := &recordingModel{scriptedModel: scriptedModel{responses: []*agents.ModelResponse{
		resp(toolCallArgs(t, PlanToolName, "c1", `{"plan":"v1"}`)),
		resp(message(t, "let me rethink")),
	}}}
	agent := &agents.Agent{
		Name:      "a",
		ModelImpl: model,
		Tools:     []agents.Tool{noopTool("write_file", &writes)},
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
	if slices.Contains(last, "write_file") {
		t.Fatalf("write tool visible after a rejection: %v", last)
	}
	if !slices.Contains(last, PlanToolName) {
		t.Fatalf("submit_plan gone after a rejection: %v", last)
	}
	if writes.Load() != 0 {
		t.Fatal("a write ran after a rejection")
	}
}

// fakeMCP lists a fixed set of tools.
type fakeMCP struct{ tools []agents.Tool }

func (f fakeMCP) Name() string { return "fake" }
func (f fakeMCP) Close() error { return nil }
func (f fakeMCP) ListTools(context.Context, *agents.RunContext, *agents.Agent) ([]agents.Tool, error) {
	return slices.Clone(f.tools), nil
}

// MCP tools are listed fresh each turn; while planning only read-only names
// survive the listing, afterwards everything does.
func TestPlan_MCPListingIsPhaseGated(t *testing.T) {
	inner := fakeMCP{tools: []agents.Tool{noopTool("read_file", nil), noopTool("mcp__write", nil)}}
	phase := &PlanPhase{}
	m := planMCP{inner: inner, phase: phase, readOnly: map[string]bool{"read_file": true}}

	planning, err := m.ListTools(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(planning) != 1 || planning[0].ToolName() != "read_file" {
		t.Fatalf("planning listing = %v", toolNames(planning))
	}
	if err := phase.Unlock(); err != nil {
		t.Fatal(err)
	}
	after, err := m.ListTools(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 2 {
		t.Fatalf("execution listing = %v", toolNames(after))
	}
}

func toolNames(tools []agents.Tool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.ToolName())
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
