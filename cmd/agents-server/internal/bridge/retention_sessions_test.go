package bridge

import (
	"errors"
	"testing"
	"time"

	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// The sweep collects a hidden session no task names and, with the retention
// window set, the transcript of a task finished longer ago than the window.
func TestTaskSessionRetentionCollectsOrphansAndFinishedTranscripts(t *testing.T) {
	ctx := t.Context()
	runner, sessions, tasks, _ := newTaskTestRunner(t)
	if err := store.NewSettingStore(runner.db).Set(ctx, settings.KeyTaskSessionRetentionDays, "1"); err != nil {
		t.Fatal(err)
	}
	parent := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "p"}
	orphan := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "orphan", Hidden: true}
	finished := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "finished", Hidden: true}
	for _, s := range []*store.Session{parent, orphan, finished} {
		if err := sessions.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	row := &store.Task{ID: store.NewID(), RunID: store.NewID(), ParentSessionID: parent.ID, ChildSessionID: finished.ID, Status: "completed"}
	if err := tasks.Create(ctx, row); err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-48 * time.Hour)
	if _, err := runner.db.NewUpdate().Model((*store.Session)(nil)).Set("created_at = ?", old).Where("id = ?", orphan.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.db.NewUpdate().Model((*store.Task)(nil)).Set("updated_at = ?", old).Where("id = ?", row.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	go RunTaskSessionRetention(ctx, runner.Deps.Settings, sessions)

	deadline := time.Now().Add(5 * time.Second)
	for {
		_, oerr := sessions.Get(ctx, orphan.ID)
		_, ferr := sessions.Get(ctx, finished.ID)
		if errors.Is(oerr, store.ErrNotFound) && errors.Is(ferr, store.ErrNotFound) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("orphan = %v, finished = %v; want both collected", oerr, ferr)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := sessions.Get(ctx, parent.ID); err != nil {
		t.Fatalf("the parent conversation must survive: %v", err)
	}
	if _, err := tasks.Get(ctx, row.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the finished task's row goes with its transcript, got %v", err)
	}
}
