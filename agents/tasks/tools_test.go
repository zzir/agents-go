package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents"
)

// invoke calls a tool the way the runner would.
func invoke(t *testing.T, tool *agents.Tool, sessionID, argsJSON string) (agents.ToolResult, error) {
	t.Helper()
	inv, ok := tool, tool.OnInvoke != nil
	if !ok {
		t.Fatalf("tool %q is not invokable", tool.Name)
	}
	tc := &agents.ToolContext{RunContext: agents.NewRunContext(sessionID), ToolCallID: "call-1"}
	return inv.OnInvoke(context.Background(), tc, argsJSON)
}

func toolNamed(tools []*agents.Tool, name string) *agents.Tool {
	for _, t := range tools {
		if t.Name == name {
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

// Without an id, task_status lists the conversation's tasks — summaries only,
// newest first — and consumes no wake-up debt: the finish is still delivered.
func TestTools_StatusWithoutAnIDListsTheConversation(t *testing.T) {
	ctx := context.Background()
	var delivered []string
	h := newHarness(t, func(c *Config) {
		c.SummaryLimit = 12
		c.OnResultDelivered = func(_ context.Context, task *Task) { delivered = append(delivered, task.ID) }
	})
	first := h.spawn(t)
	second := h.spawn(t)
	h.m.OnRunFinished(ctx, h.childOf(t, first.TaskID), RunOutcome{Status: StatusCompleted, Text: strings.Repeat("y", 100)})

	res, err := invoke(t, toolNamed(h.m.Tools(nil), "task_status"), "parent", `{"task_id":"","wait_seconds":0}`)
	if err != nil {
		t.Fatal(err)
	}
	out := stringOf(res)
	if !strings.HasPrefix(out, "2 task(s)") {
		t.Fatalf("listing = %q, want both tasks counted", out)
	}
	if strings.Index(out, second.TaskID) > strings.Index(out, first.TaskID) {
		t.Fatalf("listing = %q, want newest first", out)
	}
	if !strings.Contains(out, first.TaskID+": completed") || !strings.Contains(out, second.TaskID+": working") {
		t.Fatalf("listing = %q, want each task's status", out)
	}
	if strings.Contains(out, strings.Repeat("y", 100)) {
		t.Fatalf("listing = %q, want summaries, not full results", out)
	}
	if len(delivered) != 0 {
		t.Fatalf("delivered = %v — a listing must not settle the wake-up debt", delivered)
	}
	// Another conversation's listing is empty: ids do not leak sideways.
	res, err = invoke(t, toolNamed(h.m.Tools(nil), "task_status"), "other", `{"task_id":"","wait_seconds":0}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stringOf(res), "no tasks") {
		t.Fatalf("listing for another session = %q, want none", stringOf(res))
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

func TestTools_ExposesTheFour(t *testing.T) {
	m := newHarness(t).m
	tools := m.Tools(nil)
	for _, name := range []string{"spawn_task", "task_status", "task_retry", "task_stop"} {
		if toolNamed(tools, name) == nil {
			t.Errorf("missing tool %q", name)
		}
	}
	// The parts a host composes its own surface from: the spawn tool alone,
	// and the three that name an existing task.
	if st := m.SpawnTool(nil); st == nil || st.Name != "spawn_task" {
		t.Fatalf("SpawnTool = %v", st)
	}
	tt := m.TaskTools(nil)
	if len(tt) != 3 || toolNamed(tt, "spawn_task") != nil {
		t.Fatalf("TaskTools = %v, want status/retry/stop only", tt)
	}
}

// The host says where a job of its kind stands (DescribeState) and task_status
// shows it — beside the status of one task, and on the listing's line.
func TestTools_StatusShowsTheHostsProgressLine(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, func(c *Config) {
		c.DescribeState = func(kind string, state json.RawMessage) string {
			if kind != "sequence" {
				return ""
			}
			return "step 2/3 (verify) from " + string(state)
		}
	})
	info, err := h.m.Spawn(ctx, SpawnRequest{ParentSessionID: "parent", AgentName: "worker", Input: "go", Label: "seq", Kind: "sequence", State: json.RawMessage(`{"n":2}`)})
	if err != nil {
		t.Fatal(err)
	}
	res, err := invoke(t, toolNamed(h.m.Tools(nil), "task_status"), "parent", `{"task_id":"`+info.TaskID+`","wait_seconds":0}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stringOf(res), `progress: step 2/3 (verify) from {"n":2}`) {
		t.Fatalf("status = %q, want the host's progress line", stringOf(res))
	}
	list, err := invoke(t, toolNamed(h.m.Tools(nil), "task_status"), "parent", `{"task_id":"","wait_seconds":0}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stringOf(list), "step 2/3 (verify)") || !strings.Contains(stringOf(list), "still working — do not redo its work") {
		t.Fatalf("listing = %q, want the progress line and the live-task warning", stringOf(list))
	}
}

// A retry the task's state refuses is news the model can act on — spawn a new
// task, or leave it alone — not a failure it should call again.
func TestTools_RetryRefusalIsAResult(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	info := h.spawn(t)
	h.m.OnRunFinished(ctx, h.childOf(t, info.TaskID), RunOutcome{Status: StatusCompleted, Text: "done"})

	res, err := invoke(t, toolNamed(h.m.Tools(nil), "task_retry"), "parent",
		`{"task_id":"`+info.TaskID+`"}`)
	if err != nil {
		t.Fatalf("err = %v, want the refusal as a result", err)
	}
	if !res.IsError {
		t.Error("the result is not marked as an error for the UI")
	}
	out := stringOf(res)
	// The reason AND the state: "cannot retry" alone leaves the model guessing
	// at what the task actually is.
	if !strings.Contains(out, "only a failed task can be retried") || !strings.Contains(out, "completed") {
		t.Errorf("output = %q, want the reason and the task's state", out)
	}
}

// A task id that leaked into another conversation reads as nonexistent there.
// A retry is the same boundary as status and stop: it starts a run, on someone
// else's work.
func TestTools_RetryRefusesAForeignTask(t *testing.T) {
	h := newHarness(t)
	info := h.spawn(t)
	h.fail(t, info.TaskID, "boom")

	if _, err := invoke(t, toolNamed(h.m.Tools(nil), "task_retry"), "someone-else",
		`{"task_id":"`+info.TaskID+`"}`); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if got := h.get(t, info.TaskID); got.Status != StatusFailed {
		t.Errorf("status = %s, want the task untouched", got.Status)
	}
}

// The attempt reaches the card as a field and the model as a line — but only
// once it means something.
func TestTools_RetryReportsTheAttempt(t *testing.T) {
	h := newHarness(t)
	info := h.spawn(t)
	h.fail(t, info.TaskID, "boom")

	res, err := invoke(t, toolNamed(h.m.Tools(nil), "task_retry"), "parent",
		`{"task_id":"`+info.TaskID+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Details["task_attempt"]; got != 2 {
		t.Errorf("details task_attempt = %v, want 2", got)
	}
	if out := stringOf(res); !strings.Contains(out, "attempt: 2") {
		t.Errorf("output = %q, want the attempt named", out)
	}
	// The first attempt says nothing: every task has one, and a line on all of
	// them is a line the model learns to skip.
	first, err := invoke(t, toolNamed(h.m.Tools(nil), "task_status"), "parent",
		`{"task_id":"`+h.spawn(t).TaskID+`","wait_seconds":0}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stringOf(first), "attempt:") {
		t.Errorf("output = %q, want no attempt line on a first attempt", stringOf(first))
	}
}

func stringOf(r agents.ToolResult) string {
	out := r.ModelOutput()
	if s, ok := out.(string); ok {
		return s
	}
	return ""
}

// The host tags the executing run's id on the context; spawn stamps it onto
// the task so a UI can nest the task's wake-up run under the spawning run.
func TestTools_SpawnRecordsTheParentRun(t *testing.T) {
	h := newHarness(t)
	inv := toolNamed(h.m.Tools(nil), "spawn_task")
	if inv.OnInvoke == nil {
		t.Fatal("spawn_task is not invokable")
	}
	tc := &agents.ToolContext{RunContext: agents.NewRunContext("parent"), ToolCallID: "call-1"}
	ctx := WithParentRunID(context.Background(), "run-42")
	if _, err := inv.OnInvoke(ctx, tc, `{"agent_name":"worker","input":"do it","label":"job"}`); err != nil {
		t.Fatal(err)
	}
	live, _ := h.store.ListNonTerminal(context.Background(), "parent")
	if len(live) != 1 || live[0].ParentRunID != "run-42" {
		t.Errorf("task = %+v, want ParentRunID run-42 recorded", live)
	}
}
