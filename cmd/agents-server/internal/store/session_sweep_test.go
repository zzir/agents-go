package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/uptrace/bun"
)

// ageSession backdates a session's created_at, past the spawn grace.
func ageSession(t *testing.T, db *bun.DB, id string, by time.Duration) {
	t.Helper()
	if _, err := db.NewUpdate().Model((*Session)(nil)).
		Set("created_at = ?", time.Now().UTC().Add(-by)).Where("id = ?", id).Exec(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// ageTask backdates a task's updated_at — when it reached its status.
func ageTask(t *testing.T, db *bun.DB, id string, by time.Duration) {
	t.Helper()
	if _, err := db.NewUpdate().Model((*Task)(nil)).
		Set("updated_at = ?", time.Now().UTC().Add(-by)).Where("id = ?", id).Exec(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func mustCreateSessions(t *testing.T, sessions *SessionStore, rows ...*Session) {
	t.Helper()
	for _, s := range rows {
		if err := sessions.Create(context.Background(), s); err != nil {
			t.Fatalf("create session %s: %v", s.Name, err)
		}
	}
}

func wantSessions(t *testing.T, sessions *SessionStore, present []*Session, gone []*Session) {
	t.Helper()
	ctx := context.Background()
	for _, s := range present {
		if _, err := sessions.Get(ctx, s.ID); err != nil {
			t.Errorf("session %s should survive: %v", s.Name, err)
		}
	}
	for _, s := range gone {
		if _, err := sessions.Get(ctx, s.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("session %s should be collected, got %v", s.Name, err)
		}
	}
}

// A hidden session is collected once no task names it as a child over a live
// edge — a stale-generation edge is no edge — while one still named, one
// younger than the grace, and a visible session without any task all stay.
func TestDeleteOrphanHiddenCollectsTheEdgeless(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessions := NewSessionStore(db)
	tasks := NewTaskStore(db)

	parent := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "parent"}
	served := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "served", Hidden: true}
	orphan := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "orphan", Hidden: true}
	fresh := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "fresh", Hidden: true}
	staleEdge := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "stale-edge", Hidden: true}
	visible := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "visible"}
	mustCreateSessions(t, sessions, parent, served, orphan, fresh, staleEdge, visible)
	for _, s := range []*Session{parent, served, orphan, staleEdge, visible} {
		ageSession(t, db, s.ID, 2*time.Hour)
	}
	if err := tasks.Create(ctx, &Task{ID: NewID(), RunID: NewID(), ParentSessionID: parent.ID, ChildSessionID: served.ID, Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	// An edge bound to a generation the child never had — what a row left
	// over from a deleted-and-recreated child looks like.
	stale := &Task{ID: NewID(), RunID: NewID(), ParentSessionID: parent.ID, ChildSessionID: staleEdge.ID, Status: "completed"}
	if _, err := db.NewInsert().Model(stale).
		Value("parent_session_gen", genOf, parent.ID).
		Value("child_session_gen", "?", "gen-of-a-former-child").
		Exec(ctx); err != nil {
		t.Fatal(err)
	}

	n, err := sessions.DeleteOrphanHidden(ctx, time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("collected %d, want 2 (the orphan and the stale edge)", n)
	}
	wantSessions(t, sessions, []*Session{parent, served, fresh, visible}, []*Session{orphan, staleEdge})
	// The stale row named the collected session as its child, so it went too.
	if _, err := tasks.Get(ctx, stale.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale task row should go with its session, got %v", err)
	}
	if n, err := sessions.DeleteOrphanHidden(ctx, time.Now().UTC().Add(-time.Hour)); err != nil || n != 0 {
		t.Fatalf("second sweep = %d, %v; want nothing left to collect", n, err)
	}
}

// Retention takes the transcript of a task terminal for longer than the
// window — its nested tasks' sessions with it — and the task row, since a
// row without its transcript answers nothing. A live task and a recently
// finished one keep theirs, and the parent conversation is untouched.
func TestDeleteTaskSessionsBeforeTakesFinishedTranscriptsAndRows(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sessions := NewSessionStore(db)
	tasks := NewTaskStore(db)

	parent := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "parent"}
	oldDone := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "old-done", Hidden: true}
	nested := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "nested", Hidden: true}
	live := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "live", Hidden: true}
	recent := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "recent", Hidden: true}
	mustCreateSessions(t, sessions, parent, oldDone, nested, live, recent)
	oldTask := &Task{ID: NewID(), RunID: NewID(), ParentSessionID: parent.ID, ChildSessionID: oldDone.ID, Status: "failed"}
	nestedTask := &Task{ID: NewID(), RunID: NewID(), ParentSessionID: oldDone.ID, ChildSessionID: nested.ID, Status: "completed"}
	liveTask := &Task{ID: NewID(), RunID: NewID(), ParentSessionID: parent.ID, ChildSessionID: live.ID, Status: "input_required"}
	recentTask := &Task{ID: NewID(), RunID: NewID(), ParentSessionID: parent.ID, ChildSessionID: recent.ID, Status: "completed"}
	for _, task := range []*Task{oldTask, nestedTask, liveTask, recentTask} {
		if err := tasks.Create(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	for _, task := range []*Task{oldTask, nestedTask, liveTask} {
		ageTask(t, db, task.ID, 48*time.Hour)
	}

	n, err := sessions.DeleteTaskSessionsBefore(ctx, time.Now().UTC().Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("collected %d, want 1 (the nested transcript goes inside the tree)", n)
	}
	wantSessions(t, sessions, []*Session{parent, live, recent}, []*Session{oldDone, nested})
	for _, task := range []*Task{oldTask, nestedTask} {
		if _, err := tasks.Get(ctx, task.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("task %s should go with its transcript, got %v", task.ID, err)
		}
	}
	for _, task := range []*Task{liveTask, recentTask} {
		if _, err := tasks.Get(ctx, task.ID); err != nil {
			t.Errorf("task %s should stay: %v", task.ID, err)
		}
	}
	if got, err := tasks.ListByParent(ctx, parent.ID); err != nil || len(got) != 2 {
		t.Fatalf("parent lists %d tasks (%v), want the two kept", len(got), err)
	}
}
