package bridge

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/agents/session"
	sdktasks "github.com/zzir/agents-go/agents/tasks"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

func TestTaskStatusForMapsAllStates(t *testing.T) {
	want := map[RunStatus]string{
		RunRunning:     protocol.TaskWorking,
		RunInterrupted: protocol.TaskInputRequired,
		RunCompleted:   protocol.TaskCompleted,
		RunErrored:     protocol.TaskFailed,
		RunCancelled:   protocol.TaskCancelled,
	}
	for rs, ts := range want {
		if got := TaskStatusFor(rs); got != ts {
			t.Errorf("TaskStatusFor(%s) = %s, want %s", rs, got, ts)
		}
	}
}

func newTaskTestRunner(t *testing.T) (*Runner, *store.SessionStore, *store.TaskStore, *store.AgentConfigStore) {
	t.Helper()
	db := newTestDB(t)
	sessions := store.NewSessionStore(db)
	tasks := store.NewTaskStore(db)
	agentConfigs := store.NewAgentConfigStore(db)
	runner := NewRunner(context.Background(), db, &AgentDeps{
		AgentConfigs:     agentConfigs,
		Providers:        store.NewProviderStore(db),
		Sessions:         sessions,
		Settings:         settings.NewReader(store.NewSettingStore(db)),
		Memories:         store.NewMemoryStore(db),
		PendingApprovals: store.NewPendingApprovalStore(db),
		Tasks:            tasks,
		// Wired here rather than per test: a run's spans are persisted on the
		// run's own goroutine, so a nil store panics the test binary from a
		// stack that has nothing to do with what the test is checking. The
		// wake-up debts are the same kind of trap.
		Traces:  store.NewTraceStore(db),
		Wakeups: store.NewWakeupStore(db),
	})
	return runner, sessions, tasks, agentConfigs
}

// TestSpawnTaskCreatesHiddenSessionAndRow locks the spawn contract: a task
// gets its own child session (hidden from the chat list), a durable row with
// its own run id, and a live hub run carrying the task linkage.
func TestSpawnTaskCreatesHiddenSessionAndRow(t *testing.T) {
	ctx := context.Background()
	runner, sessions, tasks, agentConfigs := newTaskTestRunner(t)

	ac := &store.AgentConfig{Name: "worker", Model: "gpt-test"}
	if err := agentConfigs.Create(ctx, ac); err != nil {
		t.Fatal(err)
	}
	parent := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "chat"}
	if err := sessions.Create(ctx, parent); err != nil {
		t.Fatal(err)
	}

	// Spawn through the SDK manager — the same path the spawn_task tool takes.
	info, err := runner.Tasks().Spawn(ctx, sdktasks.SpawnRequest{
		ParentSessionID: parent.ID,
		AgentName:       "worker",
		Input:           "audit the code",
		Label:           "audit",
		ToolCallID:      "call-42",
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if string(info.Status) != protocol.TaskWorking || info.TaskID == "" {
		t.Fatalf("info = %+v", info)
	}

	task, err := tasks.Get(ctx, info.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.ParentSessionID != parent.ID || task.ToolCallID != "call-42" || task.Label != "audit" {
		t.Fatalf("task row = %+v", task)
	}

	// The child session exists but is hidden from the chat session list.
	if _, err := sessions.Get(ctx, task.ChildSessionID); err != nil {
		t.Fatalf("child session missing: %v", err)
	}
	list, err := sessions.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range list {
		if s.ID == task.ChildSessionID {
			t.Fatal("task child session leaked into the chat session list")
		}
	}

	// Task identity and run attempt are separate ids; the hub run lives under
	// the run id and carries the task linkage in its meta.
	if task.RunID == "" || task.RunID == task.ID {
		t.Fatalf("task.RunID = %q, want a distinct run id (task id %s)", task.RunID, task.ID)
	}
	runInfo, ok := runner.Hub().Info(task.RunID)
	if !ok {
		t.Fatal("no hub run for task")
	}
	if runInfo.Task == nil || runInfo.Task.ParentSessionID != parent.ID || runInfo.Task.TaskID != task.ID {
		t.Fatalf("hub run task meta = %+v", runInfo.Task)
	}
	if runner.Hub().LiveTaskCount(parent.ID) != 1 {
		t.Fatalf("LiveTaskCount = %d, want 1", runner.Hub().LiveTaskCount(parent.ID))
	}
}

// TestDrainTaskNotificationsQueuesWhileBusy locks the wake semantics: a
// finished task notifies at the parent's run boundary — while the parent has a
// live run the notification stays queued; once free, a notification run
// starts and persists the task-notification prompt.
func TestDrainTaskNotificationsQueuesWhileBusy(t *testing.T) {
	ctx := context.Background()
	runner, sessions, tasks, agentConfigs := newTaskTestRunner(t)

	ac := &store.AgentConfig{Name: "chat-agent", Model: "gpt-test"}
	if err := agentConfigs.Create(ctx, ac); err != nil {
		t.Fatal(err)
	}
	parent := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "chat", AgentConfigID: ac.ID}
	if err := sessions.Create(ctx, parent); err != nil {
		t.Fatal(err)
	}
	task := &store.Task{
		ID: store.NewID(), RunID: store.NewID(), ParentSessionID: parent.ID, ChildSessionID: store.NewID(),
		Label: "audit", Status: protocol.TaskWorking,
		ParentAgentConfigID: ac.ID,
	}
	if err := tasks.Create(ctx, task); err != nil {
		t.Fatal(err)
	}
	// Finalize through the adapter: the wake-up debt is written in the SAME
	// transaction as the terminal state — no window where the task is done but
	// its parent is owed nothing.
	adapter := store.NewTaskAdapter(tasks)
	if won, err := adapter.Finalize(ctx, task.ID, task.RunID, sdktasks.StatusCompleted, "all green", "", nil); err != nil || !won {
		t.Fatalf("Finalize won=%v err=%v", won, err)
	}
	// Busy parent: the debt stays pending — draining refuses while a run holds
	// the session, and taskFinished (OnFinished) now only drains.
	if _, _, err := runner.hub.register("busy-run", parent.ID, "", ac.ID, "", "", nil); err != nil {
		t.Fatal(err)
	}
	runner.taskFinished(ctx, &sdktasks.Task{
		ID: task.ID, RunID: task.RunID, ParentSessionID: parent.ID, Label: task.Label,
		Status: sdktasks.StatusCompleted, Summary: "all green",
		Inherit: store.EncodeInherit(store.Inherit{AgentConfigID: ac.ID}),
	})
	pending, err := runner.Deps.Wakeups.Pending(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("%d debts pending while the parent is busy, want 1", len(pending))
	}

	// Free the parent: the drain starts a notification run. The test config has
	// no API key, so the run fails at the provider stage — but that failure
	// path persists the prompt, which is exactly the observable we need.
	runner.hub.finish("busy-run", false)
	(Waker{runner}).Drain(ctx, parent.ID)

	// The notification run executes on a background goroutine; poll for its
	// persisted prompt (the keyless test config fails at the provider stage,
	// and that failure path saves the prompt).
	entries := store.NewSharedEntryStore(runner.db)
	found := false
	deadline := time.Now().Add(5 * time.Second)
	for !found && time.Now().Before(deadline) {
		rows, err := entries.GetEntries(ctx, mustRef(t, runner.db, parent.ID), 0, 50)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range rows {
			if strings.HasPrefix(m.Content, protocol.TaskNotificationPrefix) && strings.Contains(m.Content, "audit") {
				found = true
			}
		}
		if !found {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !found {
		t.Fatal("no task-notification prompt persisted")
	}
	settled, err := runner.Deps.Wakeups.Pending(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(settled) != 0 {
		t.Fatalf("%d debts still pending after the drain, want 0", len(settled))
	}
}

// TestStartupSweepDeliversPendingNotifications locks the restart contract: a
// wake-up owed before the restart (here: an orphaned working task the boot
// reconciliation marks failed) is delivered by the startup sweep.
func TestStartupSweepDeliversPendingNotifications(t *testing.T) {
	ctx := context.Background()
	runner, sessions, tasks, agentConfigs := newTaskTestRunner(t)

	ac := &store.AgentConfig{Name: "chat-agent", Model: "gpt-test"}
	if err := agentConfigs.Create(ctx, ac); err != nil {
		t.Fatal(err)
	}
	parent := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "chat", AgentConfigID: ac.ID}
	if err := sessions.Create(ctx, parent); err != nil {
		t.Fatal(err)
	}
	task := &store.Task{
		ID: store.NewID(), RunID: store.NewID(), ParentSessionID: parent.ID, ChildSessionID: store.NewID(),
		Label: "orphaned", Status: protocol.TaskWorking, ParentAgentConfigID: ac.ID,
	}
	if err := tasks.Create(ctx, task); err != nil {
		t.Fatal(err)
	}

	// Boot sequence: reconcile orphans (working -> failed, each reported so a
	// debt is recorded), then sweep what is owed.
	runner.FailOrphanedTasks(ctx)
	runner.DrainPendingWakeups(ctx)

	entries := store.NewSharedEntryStore(runner.db)
	found := false
	deadline := time.Now().Add(5 * time.Second)
	for !found && time.Now().Before(deadline) {
		rows, err := entries.GetEntries(ctx, mustRef(t, runner.db, parent.ID), 0, 50)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range rows {
			if strings.HasPrefix(m.Content, protocol.TaskNotificationPrefix) && strings.Contains(m.Content, "orphaned") && strings.Contains(m.Content, "failed") {
				found = true
			}
		}
		if !found {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !found {
		t.Fatal("startup sweep did not deliver the owed notification")
	}
}

// TestBuildFullAgentTaskDepthCap locks the one-level spawn depth: chat agents
// get the task tools, task-run agents do not.
func TestBuildFullAgentTaskDepthCap(t *testing.T) {
	ctx := context.Background()
	runner, _, _, agentConfigs := newTaskTestRunner(t)

	ac := &store.AgentConfig{Name: "worker", Model: "gpt-test"}
	if err := agentConfigs.Create(ctx, ac); err != nil {
		t.Fatal(err)
	}

	chat, err := buildFullAgent(ctx, runner.Deps, ac.ID, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !hasTool(chat, "spawn_task") {
		t.Fatal("chat agent missing spawn_task")
	}
	taskAgent, err := buildFullAgent(ctx, runner.Deps, ac.ID, "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if hasTool(taskAgent, "spawn_task") {
		t.Fatal("task agent must not get spawn_task (depth cap)")
	}
}

func hasTool(b *BuildResult, name string) bool {
	for _, tool := range b.Agent.Tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

// mustRef addresses a session the way production code does: by resolving its
// generation. A test that reaches for session.Direct is asking for the scope of
// the constructors where an id names the storage, which a repo-created session
// is not in.
func mustRef(t *testing.T, db *bun.DB, id string) session.Ref {
	t.Helper()
	ref, err := store.RefFor(context.Background(), db, id)
	if err != nil {
		t.Fatalf("resolving session %s: %v", id, err)
	}
	return ref
}

// testProvider creates a provider row and returns its id, for agent configs
// that need a working endpoint.
func testProvider(t *testing.T, db *bun.DB, name, apiKey, baseURL string) string {
	t.Helper()
	pv := &store.Provider{Name: name, APIKey: apiKey, BaseURL: baseURL}
	if err := store.NewProviderStore(db).Create(context.Background(), pv); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	return pv.ID
}
