package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzir/agents-go/agents/tasks"
	"github.com/zzir/agents-go/cmd/agents-server/internal/sandboxes"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/sandbox"
)

// commandFixture is a runner with a workspace on a local sandbox under dir
// and an agent opted into background commands, on the model at url.
type commandFixture struct {
	runner    *Runner
	sessions  *store.SessionStore
	tasks     *store.TaskStore
	agents    *store.AgentConfigStore
	agent     *store.AgentConfig
	workspace *store.Workspace
	dir       string
}

func newCommandFixture(t *testing.T, url string) *commandFixture {
	t.Helper()
	ctx := context.Background()
	runner, sessions, taskRows, agentConfigs := newTaskTestRunner(t)
	dir := t.TempDir()
	runner.Deps.Sandboxes = store.NewSandboxStore(runner.db)
	runner.Deps.Workspaces = store.NewWorkspaceStore(runner.db)
	runner.Deps.SandboxManager = sandboxes.NewManager()
	runner.Deps.SandboxManager.SetBuildOverride(func(sandboxes.Spec) (sandbox.Sandbox, error) {
		return sandbox.NewLocalWithOptions(sandbox.LocalOptions{WorkDir: dir}), nil
	})
	tg := &store.Sandbox{ID: store.NewID(), Name: "host", Type: "docker", Config: []byte(`{"image":"i"}`)}
	if err := runner.Deps.Sandboxes.Create(ctx, tg); err != nil {
		t.Fatal(err)
	}
	ws := &store.Workspace{OwnerID: store.LocalUserID, SandboxID: tg.ID, Name: "p"}
	if err := runner.Deps.Workspaces.Create(ctx, ws); err != nil {
		t.Fatal(err)
	}
	ac := &store.AgentConfig{OwnerID: store.LocalUserID, Name: "operator", Model: "gpt-test",
		ProviderID: testProvider(t, runner.db, "endpoint", "k", url),
		Behavior:   store.BehaviorGroup{BackgroundCommands: true}}
	if err := agentConfigs.Create(ctx, ac); err != nil {
		t.Fatal(err)
	}
	return &commandFixture{runner, sessions, taskRows, agentConfigs, ac, ws, dir}
}

// backgroundModel asks for cmd in the background on its first call and says a
// word on every other — the chat turn's close and the wake-up turn.
func backgroundModel(t *testing.T, cmd string) (*recordingModel, string) {
	t.Helper()
	args, _ := json.Marshal(map[string]any{"cmd": cmd, "background": true})
	model, srv := newRecordingModel(t, func(call int, _ []byte) []any {
		if call == 1 {
			return callOutput("call_1", "exec_command", string(args))
		}
		return sayOutput("noted")
	})
	return model, srv.URL
}

// chat runs one turn on a fresh session bound to the workspace and waits for it.
func (f *commandFixture) chat(t *testing.T) *store.Session {
	t.Helper()
	ctx := context.Background()
	sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "chat", WorkspaceID: f.workspace.ID}
	if err := f.sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	done := make(chan *RunOutcome, 1)
	if _, err := f.runner.StartRun(sess.ID, f.agent.ID, f.workspace.ID, TextInput("build it"), nil, func(o *RunOutcome) { done <- o }); err != nil {
		t.Fatal(err)
	}
	select {
	case out := <-done:
		if out.ErrCode != "" {
			t.Fatalf("the turn failed: %s %s", out.ErrCode, out.ErrMessage)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the turn did not finish")
	}
	return sess
}

// task is the session's one task row.
func (f *commandFixture) task(t *testing.T, sessionID string) store.Task {
	t.Helper()
	rows, err := f.tasks.ListByParent(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("tasks = %d, want the one command", len(rows))
	}
	return rows[0]
}

// idle waits for the session to have no live run (the wake-up turn is over).
func (f *commandFixture) idle(t *testing.T, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, live := f.runner.hub.ActiveRunForSession(sessionID); !live {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the session never went idle")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// An exec_command call with background: true becomes a task of kind command
// on the call's own card: it runs on the workspace's sandbox, its output lands
// under the spill directory, a non-zero exit fails it, the verdict wakes the
// parent, and it cannot be retried — invariant 96.
func TestBackgroundCommandRunsAsATask(t *testing.T) {
	const cmd = "sleep 0.3; echo hello; echo oops >&2; exit 3"
	model, url := backgroundModel(t, cmd)
	f := newCommandFixture(t, url)
	sess := f.chat(t)

	row := f.task(t, sess.ID)
	if row.Kind != store.TaskKindCommand || row.ToolCallID != "call_1" || row.Label != cmd {
		t.Fatalf("row = kind %q call %q label %q", row.Kind, row.ToolCallID, row.Label)
	}
	if got := awaitTaskStatus(t, f.tasks, row.ID, 10*time.Second); got != "failed" {
		t.Fatalf("status = %q, want failed for exit 3", got)
	}
	row = f.task(t, sess.ID)
	if !strings.HasPrefix(row.Summary, "exit 3") || !strings.Contains(row.Result, "hello") || !strings.Contains(row.Result, "oops") {
		t.Errorf("summary %q / result %q: want the verdict first and both streams", row.Summary, row.Result)
	}
	// The whole output is in the sandbox, not only what reached the model.
	saved, _ := filepath.Glob(filepath.Join(f.dir, ".agents", "exec", "*-call_1.stdout"))
	if len(saved) != 1 {
		t.Fatalf("saved stdout files = %v, want one", saved)
	}
	if b, _ := os.ReadFile(saved[0]); strings.TrimSpace(string(b)) != "hello" {
		t.Errorf("saved stdout = %q", b)
	}
	// The ending woke the parent with the verdict.
	model.toolsOfBody(t, "exit 3", 10*time.Second)
	f.idle(t, sess.ID)

	if _, err := f.runner.RetryTask(row.ID); err == nil {
		t.Error("a command task was retried; want the one-attempt refusal")
	} else if _, ok := errors.AsType[tasks.ErrRetryLimit](err); !ok {
		t.Errorf("retry refused with %v, want ErrRetryLimit", err)
	}
	if got := f.runner.MaxTaskAttempts(store.TaskKindCommand); got != 1 {
		t.Errorf("MaxTaskAttempts(command) = %d, want 1", got)
	}
}

// A stop kills the command and the row says cancelled.
func TestBackgroundCommandStopKillsIt(t *testing.T) {
	const marker = "sleep 986412"
	_, url := backgroundModel(t, marker)
	f := newCommandFixture(t, url)
	sess := f.chat(t)
	row := f.task(t, sess.ID)
	if !awaitProcess(marker, true, 10*time.Second) {
		t.Fatal("the command never started")
	}
	stopped, err := f.runner.StopTask(row.ID, false)
	if err != nil {
		t.Fatalf("StopTask: %v", err)
	}
	if stopped.Status != "cancelled" {
		t.Errorf("stop answered %q, want cancelled", stopped.Status)
	}
	if !awaitProcess(marker, false, 10*time.Second) {
		t.Error("the command survived the stop")
	}
	if got := awaitTaskStatus(t, f.tasks, row.ID, 5*time.Second); got != "cancelled" {
		t.Errorf("row = %q, want cancelled", got)
	}
	f.idle(t, sess.ID)
}

// A sandbox stop ends the workspace's commands first and returns once their
// leases are gone, so the compute is free to stop.
func TestStopWorkspaceCommandsEndsThem(t *testing.T) {
	const marker = "sleep 986413"
	_, url := backgroundModel(t, marker)
	f := newCommandFixture(t, url)
	sess := f.chat(t)
	row := f.task(t, sess.ID)
	if !awaitProcess(marker, true, 10*time.Second) {
		t.Fatal("the command never started")
	}
	f.runner.StopWorkspaceCommands(context.Background(), f.workspace.ID)
	if awaitProcess(marker, true, 0) {
		t.Error("the command outlived StopWorkspaceCommands")
	}
	after, err := f.tasks.Get(context.Background(), row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != "cancelled" {
		t.Errorf("row = %q right after the stop, want cancelled", after.Status)
	}
	if runs := f.runner.commands.ofWorkspace(f.workspace.ID); len(runs) != 0 {
		t.Errorf("%d commands still registered for the workspace", len(runs))
	}
	f.idle(t, sess.ID)
}

// The background flag is offered on a chat run of an agent that opted in, and
// on nothing else: not on a background run, not without the toggle.
func TestBackgroundCommandOfferedToChatRunsThatOptIn(t *testing.T) {
	ctx := context.Background()
	_, url := backgroundModel(t, "true")
	f := newCommandFixture(t, url)
	plain := &store.AgentConfig{OwnerID: store.LocalUserID, Name: "plain", Model: "gpt-test", ProviderID: f.agent.ProviderID}
	if err := f.agents.Create(ctx, plain); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name       string
		agent      string
		background bool
		want       bool
	}{
		{"chat run, opted in", f.agent.ID, false, true},
		{"background run, opted in", f.agent.ID, true, false},
		{"chat run, not opted in", plain.ID, false, false},
	}
	for _, tc := range cases {
		built, err := buildFullAgent(ctx, f.runner.Deps, tc.agent, f.workspace.ID, tc.background, store.LocalUserID)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := execOffersBackground(t, built); got != tc.want {
			t.Errorf("%s: exec_command offers background = %v, want %v", tc.name, got, tc.want)
		}
		built.Release()
	}
}

// execOffersBackground reports whether the build's exec_command schema has the
// background property.
func execOffersBackground(t *testing.T, built *BuildResult) bool {
	t.Helper()
	for _, tool := range built.Agent.Tools {
		if tool.Name != "exec_command" {
			continue
		}
		props, _ := tool.ParamsJSONSchema["properties"].(map[string]any)
		_, ok := props["background"]
		return ok
	}
	t.Fatal("the build has no exec_command")
	return false
}
