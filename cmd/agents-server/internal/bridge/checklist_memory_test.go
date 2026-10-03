package bridge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// A task's checklist lands in the TASK's own session memory, not the parent's
// the run context names: the parent's panel reads it from there, and a reset
// carries it into the task's next context (invariant 91).
func TestChecklistWritesThroughToItsOwnSession(t *testing.T) {
	ctx := context.Background()
	const list = `{"todos":[{"content":"read the code","status":"completed"},{"content":"write the fix","status":"in_progress"},{"content":"run the tests","status":"pending"}]}`
	_, srv := newRecordingModel(t, func(_ int, body []byte) []any {
		switch {
		case strings.Contains(string(body), `"function_call_output"`):
			return sayOutput("ok")
		case strings.Contains(string(body), "running in the background"):
			return callOutput("call_todo", ChecklistToolName, list)
		default:
			return callOutput("call_spawn", SpawnToolName, `{"agent_name":"","workflow":"","input":"do it","label":"t"}`)
		}
	})
	defer srv.Close()
	runner, sessions, tasks, agentConfigs := newTaskTestRunner(t)
	ac := &store.AgentConfig{OwnerID: store.LocalUserID, Name: "worker", Model: "gpt-test",
		ProviderID: testProvider(t, runner.db, "endpoint", "k", srv.URL), Behavior: store.BehaviorGroup{Checklist: true}}
	if err := agentConfigs.Create(ctx, ac); err != nil {
		t.Fatal(err)
	}
	parent := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "chat"}
	if err := sessions.Create(ctx, parent); err != nil {
		t.Fatal(err)
	}
	if out := runChat(t, runner, parent, ac, "go"); out.ErrCode != "" {
		t.Fatalf("chat run failed: %s %s", out.ErrCode, out.ErrMessage)
	}
	rows, err := tasks.ListByParent(ctx, parent.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("tasks = %v, %v; want the one spawned", rows, err)
	}
	if status := awaitTaskStatus(t, tasks, rows[0].ID, 15*time.Second); status != "completed" {
		t.Fatalf("task status = %q, want completed", status)
	}
	childRef, err := store.RefFor(ctx, runner.db, rows[0].ChildSessionID)
	if err != nil {
		t.Fatal(err)
	}
	kept, err := runner.Deps.Memories.GetByKey(ctx, store.SessionMemoryScope(childRef), store.ChecklistKey)
	if err != nil {
		t.Fatalf("the task's session has no checklist.md: %v", err)
	}
	want := "- [x] read the code\n- [~] write the fix (in progress)\n- [ ] run the tests\n"
	if kept.Content != want || kept.Metadata != store.ChecklistSource || kept.WrittenBy != store.MemoryWrittenByModel {
		t.Fatalf("checklist.md = %q (%s, by %s), want %q", kept.Content, kept.Metadata, kept.WrittenBy, want)
	}
	parentRef, err := store.RefFor(ctx, runner.db, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Deps.Memories.GetByKey(ctx, store.SessionMemoryScope(parentRef), store.ChecklistKey); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the parent session got the task's checklist: %v", err)
	}
	// The hidden session is the parent's owner's, so the owner reads it.
	child, err := sessions.Get(ctx, rows[0].ChildSessionID)
	if err != nil || child.OwnerID != parent.OwnerID {
		t.Fatalf("child session owner = %v (%v), want the parent's", child, err)
	}
}
