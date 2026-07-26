package tasks

import (
	"context"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents"
)

// invoke calls a tool the way the runner would.
func invoke(t *testing.T, tool agents.Tool, sessionID, argsJSON string) (agents.ToolResult, error) {
	t.Helper()
	inv, ok := agents.ToolAs[agents.InvokableTool](tool)
	if !ok {
		t.Fatalf("tool %q is not invokable", tool.ToolName())
	}
	tc := &agents.ToolContext{RunContext: agents.NewRunContext(sessionID), ToolCallID: "call-1"}
	return inv.Invoke(context.Background(), tc, argsJSON)
}

func toolNamed(tools []agents.Tool, name string) agents.Tool {
	for _, t := range tools {
		if t.ToolName() == name {
			return t
		}
	}
	return nil
}

func TestTools_SpawnReturnsImmediately(t *testing.T) {
	h := newHarness(t)
	tools := h.m.Tools(nil)

	res, err := invoke(t, toolNamed(tools, "spawn_task"), "parent",
		`{"agent_name":"worker","input":"do it","label":"job"}`)
	if err != nil {
		t.Fatal(err)
	}
	out := stringOf(res)
	if !strings.Contains(out, "task_id:") || !strings.Contains(out, "working") {
		t.Errorf("model output = %q, want the id and status", out)
	}
	// The UI gets fields, not the model's prose: a renderer parsing text back
	// into fields is how the two drift apart.
	if res.Display != "task" {
		t.Errorf("display = %q, want task", res.Display)
	}
	if res.Details["task_status"] != "working" {
		t.Errorf("details = %v, want the status as a field", res.Details)
	}
	// The spawning call id rides along, so the task's later state changes can
	// reach the card this call produced.
	live, _ := h.store.ListNonTerminal(context.Background(), "parent")
	if len(live) != 1 || live[0].ToolCallID != "call-1" {
		t.Errorf("task = %+v, want the spawning call id recorded", live)
	}
}

// The session comes from the run context, not the model: otherwise one
// conversation could spawn tasks onto another.
func TestTools_SpawnRefusesWithoutASession(t *testing.T) {
	h := newHarness(t)
	_, err := invoke(t, toolNamed(h.m.Tools(nil), "spawn_task"), "",
		`{"agent_name":"worker","input":"do it"}`)
	if err == nil {
		t.Fatal("a spawn without a session was accepted")
	}
}

// task_status is the call that exists to fetch the full result; the
// notification only carried a summary.
func TestTools_StatusReturnsTheFullResult(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, func(c *Config) { c.SummaryLimit = 10 })
	info := h.spawn(t)
	long := strings.Repeat("y", 400)
	h.m.OnRunFinished(ctx, h.childOf(t, info.TaskID), RunOutcome{Status: StatusCompleted, Text: long})

	res, err := invoke(t, toolNamed(h.m.Tools(nil), "task_status"), "parent",
		`{"task_id":"`+info.TaskID+`","wait_seconds":0}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stringOf(res), long) {
		t.Error("task_status did not return the full result")
	}
}

// Stopping something already finished is news, not a failure the model should
// retry.
func TestTools_StopOfAFinishedTaskIsAResult(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	info := h.spawn(t)
	h.m.OnRunFinished(ctx, h.childOf(t, info.TaskID), RunOutcome{Status: StatusCompleted, Text: "done"})

	res, err := invoke(t, toolNamed(h.m.Tools(nil), "task_stop"), "parent",
		`{"task_id":"`+info.TaskID+`","graceful":false}`)
	if err != nil {
		t.Fatalf("err = %v, want the terminal state as a result", err)
	}
	if !res.IsError {
		t.Error("the result is not marked as an error for the UI")
	}
	if !strings.Contains(stringOf(res), "completed") {
		t.Errorf("output = %q, want the terminal status", stringOf(res))
	}
}

func TestTools_ExposesTheThree(t *testing.T) {
	tools := newHarness(t).m.Tools(nil)
	for _, name := range []string{"spawn_task", "task_status", "task_stop"} {
		if toolNamed(tools, name) == nil {
			t.Errorf("missing tool %q", name)
		}
	}
}

func stringOf(r agents.ToolResult) string {
	out := r.ModelOutput()
	if s, ok := out.(string); ok {
		return s
	}
	return ""
}
