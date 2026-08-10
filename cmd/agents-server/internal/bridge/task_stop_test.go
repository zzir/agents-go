package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"testing"
	"time"

	sdktasks "github.com/zzir/agents-go/agents/tasks"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// sseWriter is the little bit of Responses-API shape these tests need: a
// streaming reply the SDK can parse.
func sseWriter(w http.ResponseWriter) func(event string, payload any) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	return func(event string, payload any) {
		b, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func sseCreated(send func(string, any), turn int) {
	send("response.created", map[string]any{
		"type": "response.created", "sequence_number": 0,
		"response": map[string]any{
			"id": fmt.Sprintf("resp_%d", turn), "object": "response", "created_at": 0,
			"status": "in_progress", "model": "gpt-test", "output": []any{},
		},
	})
}

// endlessModel streams text forever, so a run using it ends only when its
// context is cancelled. arrived reports the first request; gone closes when
// that request's context dies.
type endlessModel struct {
	arrived chan struct{}
	gone    chan struct{}
}

func (m *endlessModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case m.arrived <- struct{}{}:
	default:
	}
	send := sseWriter(w)
	sseCreated(send, 1)
	for i := 1; ; i++ {
		select {
		case <-r.Context().Done():
			close(m.gone)
			return
		case <-time.After(50 * time.Millisecond):
		}
		send("response.output_text.delta", map[string]any{
			"type": "response.output_text.delta", "sequence_number": i,
			"item_id": "msg_1", "output_index": 0, "content_index": 0, "delta": "tick ",
		})
	}
}

// toolThenSilence answers the first call with a function_call the run must
// execute, and hangs on every call after it.
type toolThenSilence struct {
	calls   chan int
	turns   chan struct{}
	command string
}

func (m *toolThenSilence) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	first := false
	select {
	case m.turns <- struct{}{}:
		first = true
	default:
	}
	send := sseWriter(w)
	sseCreated(send, 1)
	select {
	case m.calls <- 1:
	default:
	}
	if !first {
		<-r.Context().Done()
		return
	}
	args, _ := json.Marshal(map[string]any{"cmd": m.command, "timeout_seconds": 0, "workdir": "", "session_id": ""})
	send("response.completed", map[string]any{
		"type": "response.completed", "sequence_number": 1,
		"response": map[string]any{
			"id": "resp_1", "object": "response", "created_at": 0, "status": "completed", "model": "gpt-test",
			"output": []any{map[string]any{
				"type": "function_call", "id": "fc_1", "call_id": "call_1",
				"name": "exec_command", "arguments": string(args), "status": "completed",
			}},
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
		},
	})
}

// fakeModelAgent registers an agent config whose provider points at srv.
func fakeModelAgent(t *testing.T, configs *store.AgentConfigStore, url string) {
	t.Helper()
	ac := &store.AgentConfig{
		Name:     "worker",
		Model:    "gpt-test",
		Provider: store.ProviderGroup{ProviderType: "openai", APIKey: "k", BaseURL: url},
	}
	if err := configs.Create(context.Background(), ac); err != nil {
		t.Fatal(err)
	}
}

// awaitTaskStatus waits for the durable row to leave "working".
func awaitTaskStatus(t *testing.T, tasks *store.TaskStore, taskID string, within time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		row, err := tasks.Get(context.Background(), taskID)
		if err != nil {
			t.Fatal(err)
		}
		if row.Status != "working" {
			return row.Status
		}
		if time.Now().After(deadline) {
			return row.Status
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestStopTaskCancelsALiveRun locks the plain case: the REST stop reaches the
// model call itself, not just the row. The row moving to cancelled while the
// run streamed on is the shape of the bug this guards — the UI shows a stopped
// task, the tokens keep being spent.
func TestStopTaskCancelsALiveRun(t *testing.T) {
	ctx := context.Background()
	model := &endlessModel{arrived: make(chan struct{}, 1), gone: make(chan struct{})}
	srv := httptest.NewServer(model)
	defer srv.Close()

	runner, sessions, tasks, agentConfigs := newTaskTestRunner(t)
	runner.Deps.Traces = store.NewTraceStore(runner.db)
	fakeModelAgent(t, agentConfigs, srv.URL)
	parent := &store.Session{ID: store.NewID(), Name: "chat"}
	if err := sessions.Create(ctx, parent); err != nil {
		t.Fatal(err)
	}

	info, err := runner.Tasks().Spawn(ctx, sdktasks.SpawnRequest{
		ParentSessionID: parent.ID, AgentName: "worker", Input: "run forever", Label: "probe",
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	select {
	case <-model.arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("the task run never reached the model")
	}
	time.Sleep(200 * time.Millisecond) // unambiguously mid-turn

	stopped, err := runner.StopTask(info.TaskID, false)
	if err != nil {
		t.Fatalf("StopTask: %v", err)
	}
	if stopped.Status != "cancelled" {
		t.Errorf("stop answered %q, want cancelled", stopped.Status)
	}
	select {
	case <-model.gone:
	case <-time.After(10 * time.Second):
		t.Fatal("the task's model request was still streaming after the stop")
	}
	if got := awaitTaskStatus(t, tasks, info.TaskID, 5*time.Second); got != "cancelled" {
		t.Errorf("row = %q after the stop, want cancelled", got)
	}
}

// TestStopTaskCancelsARunInsideATool locks the other half: a stop that lands
// while the task is inside a long command kills the command too. A tool is
// invoked on the run's own goroutine, so nothing here is reached until it
// returns — the cancellation has to travel through the tool's context.
func TestStopTaskCancelsARunInsideATool(t *testing.T) {
	ctx := context.Background()
	// A distinctive duration, so pgrep finds this test's command and no other.
	const marker = "sleep 986411"
	model := &toolThenSilence{
		calls:   make(chan int, 4),
		turns:   make(chan struct{}, 1),
		command: marker,
	}
	srv := httptest.NewServer(model)
	defer srv.Close()

	runner, sessions, tasks, agentConfigs := newTaskTestRunner(t)
	workspace := t.TempDir()
	runner.Deps.Traces = store.NewTraceStore(runner.db)
	runner.Deps.SandboxConfigs = store.NewSandboxStore(runner.db)
	runner.Deps.SandboxManager = NewSandboxManager(workspace)
	sb := &store.SandboxConfig{ID: store.NewID(), Name: "local", Type: "local"}
	if err := runner.Deps.SandboxConfigs.Create(ctx, sb); err != nil {
		t.Fatal(err)
	}
	fakeModelAgent(t, agentConfigs, srv.URL)
	parent := &store.Session{ID: store.NewID(), Name: "chat", SandboxID: sb.ID, WorkDir: workspace}
	if err := sessions.Create(ctx, parent); err != nil {
		t.Fatal(err)
	}

	info, err := runner.Tasks().Spawn(ctx, sdktasks.SpawnRequest{
		ParentSessionID: parent.ID, AgentName: "worker", Input: "run a long command", Label: "probe",
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	select {
	case <-model.calls:
	case <-time.After(10 * time.Second):
		t.Fatal("the task run never reached the model")
	}
	// The command has to be running for this test to be about anything.
	if !awaitProcess(marker, true, 10*time.Second) {
		t.Fatal("the task's command never started")
	}

	if _, err := runner.StopTask(info.TaskID, false); err != nil {
		t.Fatalf("StopTask: %v", err)
	}
	if !awaitProcess(marker, false, 10*time.Second) {
		t.Error("the task's command survived the stop")
	}
	if got := awaitTaskStatus(t, tasks, info.TaskID, 5*time.Second); got != "cancelled" {
		t.Errorf("row = %q after the stop, want cancelled", got)
	}
}

// awaitProcess waits for a process matching marker to be running (want=true)
// or gone (want=false).
func awaitProcess(marker string, want bool, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		out, _ := exec.Command("pgrep", "-f", marker).Output()
		if (len(out) > 0) == want {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestStopTaskClaimsAnEndingTheRunNeverRecorded is the third state, and the
// one that used to leave a task un-stoppable: the run is over as far as the hub
// is concerned, but nothing ever recorded its outcome on the row — a postRun
// whose store write failed leaves exactly this. The stop must end the task
// itself rather than answer "working" and change nothing, which showed a dead
// task as live, with a ticking duration and a Stop button that did nothing
// until the hub record aged out fifteen minutes later.
func TestStopTaskClaimsAnEndingTheRunNeverRecorded(t *testing.T) {
	ctx := context.Background()
	runner, sessions, tasks, _ := newTaskTestRunner(t)

	parent := &store.Session{ID: store.NewID(), Name: "chat"}
	if err := sessions.Create(ctx, parent); err != nil {
		t.Fatal(err)
	}
	child := &store.Session{ID: store.NewID(), Name: "task: probe", Hidden: true}
	if err := sessions.Create(ctx, child); err != nil {
		t.Fatal(err)
	}
	row := &store.Task{
		ID: store.NewID(), RunID: store.NewID(), Label: "probe",
		ParentSessionID: parent.ID, ChildSessionID: child.ID,
		Depth: 1, Attempt: 1, Status: "working",
	}
	if err := tasks.Create(ctx, row); err != nil {
		t.Fatal(err)
	}

	// A hub run whose segment has fully drained — its goroutine is gone, so
	// nothing else will ever speak for it — while the row still reads working.
	seg, _, err := runner.hub.register(row.RunID, child.ID, "", "", "", &TaskMeta{
		TaskID: row.ID, ParentSessionID: parent.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	runner.hub.finish(row.RunID, false)
	seg.finalize()

	stopped, err := runner.StopTask(row.ID, false)
	if err != nil {
		t.Fatalf("StopTask: %v", err)
	}
	if stopped.Status != "cancelled" {
		t.Errorf("stop answered %q, want cancelled", stopped.Status)
	}
	after, err := tasks.Get(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != "cancelled" {
		t.Errorf("row = %q after the stop, want cancelled", after.Status)
	}
}
