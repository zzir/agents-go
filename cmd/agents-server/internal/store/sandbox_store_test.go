package store

import (
	"context"
	"errors"
	"testing"
)

// The conditional delete: gone when unreferenced, refused with the blocking
// count while any session is bound, ErrNotFound for a config that never was —
// three different answers, decided by the statement itself.
func TestSandboxDeleteIfUnreferenced(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	id := ids(t)
	sessions := NewSessionStore(db)
	sandboxes := NewSandboxStore(db)
	createSandboxRow(t, db, id("sb-1"))

	sess := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "s"}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	createProjectRow(t, db, id("p-1"), id("sb-1"))
	if won, err := sessions.BindSandboxIfEmpty(ctx, sess.ID, id("sb-1"), id("p-1"), 1); err != nil || !won {
		t.Fatalf("bind: won=%v err=%v", won, err)
	}

	refs, err := sandboxes.DeleteIfUnreferenced(ctx, id("sb-1"))
	if err != nil || refs != 1 {
		t.Fatalf("referenced delete: refs=%d err=%v, want a refusal naming 1", refs, err)
	}
	if _, err := sandboxes.Get(ctx, id("sb-1")); err != nil {
		t.Fatalf("refused delete removed the row: %v", err)
	}

	if err := sessions.Delete(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	refs, err = sandboxes.DeleteIfUnreferenced(ctx, id("sb-1"))
	if err != nil || refs != 0 {
		t.Fatalf("unreferenced delete: refs=%d err=%v, want success", refs, err)
	}
	if _, err := sandboxes.Get(ctx, id("sb-1")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("row survived the delete: %v", err)
	}

	if _, err := sandboxes.DeleteIfUnreferenced(ctx, NewID()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing config: err=%v, want ErrNotFound", err)
	}
}

// The conditional identity update: applied while unreferenced, refused once a
// project row lives on the config — even before any session binds — and
// refused with both counts once one does. The id must keep meaning the same
// file system for as long as anything points at it.
func TestSandboxUpdateIdentityIfUnreferenced(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	id := ids(t)
	sessions := NewSessionStore(db)
	sandboxes := NewSandboxStore(db)
	createSandboxRow(t, db, id("sb-1"))

	moved := &SandboxConfig{Name: "sb-1", Type: "docker", Config: []byte(`{"image":"i","host":"ssh://u@other-host"}`)}
	sessRefs, projRefs, err := sandboxes.UpdateIdentityIfUnreferenced(ctx, id("sb-1"), moved, 1)
	if err != nil || sessRefs != 0 || projRefs != 0 {
		t.Fatalf("unreferenced identity update: refs=(%d,%d) err=%v, want success", sessRefs, projRefs, err)
	}
	// A second writer holding the OLD revision loses: proceeding on its stale
	// identity comparison is exactly the freeze bypass the CAS closes.
	staleWriter := &SandboxConfig{Name: "sb-1", Type: "docker", Config: []byte(`{"image":"i","host":"ssh://u@h"}`)}
	if _, _, err := sandboxes.UpdateIdentityIfUnreferenced(ctx, id("sb-1"), staleWriter, 1); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale identity update: err=%v, want ErrRevisionConflict", err)
	}

	// A project row alone pins the identity: its tree (a terminal may have
	// written files) already lives on this daemon, session or not.
	cur, err := sandboxes.Get(ctx, id("sb-1"))
	if err != nil {
		t.Fatal(err)
	}
	createProjectRow(t, db, id("p-1"), id("sb-1"))
	movedAgain := &SandboxConfig{Name: "sb-1", Type: "docker", Config: []byte(`{"image":"i","host":"ssh://u@third-host"}`)}
	sessRefs, projRefs, err = sandboxes.UpdateIdentityIfUnreferenced(ctx, id("sb-1"), movedAgain, cur.Revision)
	if err != nil || sessRefs != 0 || projRefs != 1 {
		t.Fatalf("project-held identity update: refs=(%d,%d) err=%v, want (0,1)", sessRefs, projRefs, err)
	}

	sess := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "s"}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if won, err := sessions.BindSandboxIfEmpty(ctx, sess.ID, id("sb-1"), id("p-1"), cur.Revision); err != nil || !won {
		t.Fatalf("bind: won=%v err=%v", won, err)
	}
	sessRefs, projRefs, err = sandboxes.UpdateIdentityIfUnreferenced(ctx, id("sb-1"), movedAgain, cur.Revision)
	if err != nil || sessRefs != 1 || projRefs != 1 {
		t.Fatalf("referenced identity update: refs=(%d,%d) err=%v, want (1,1)", sessRefs, projRefs, err)
	}
	got, err := sandboxes.Get(ctx, id("sb-1"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Config) != `{"image":"i","host":"ssh://u@other-host"}` {
		t.Fatalf("refused update changed the row: %s", got.Config)
	}

	if _, _, err := sandboxes.UpdateIdentityIfUnreferenced(ctx, NewID(), moved, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing config: err=%v, want ErrNotFound", err)
	}
}

// The two counters and their separation: revision moves on EVERY write (it is
// the row's concurrency control), the runtime generation only on content
// changes (it is what retires instances and severs terminals — a rename must
// move neither instance nor shell). The expected-revision CAS refuses a stale
// writer on both paths.
func TestSandboxRevisionAndGenerationBumps(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	id := ids(t)
	sandboxes := NewSandboxStore(db)
	createSandboxRow(t, db, id("sb-1"))

	got, err := sandboxes.Get(ctx, id("sb-1"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != 1 || got.RuntimeGen != 1 {
		t.Fatalf("created (revision, gen) = (%d, %d), want (1, 1)", got.Revision, got.RuntimeGen)
	}

	// A name-only write: revision moves, the generation does not.
	renamed := &SandboxConfig{Name: "renamed", Type: "docker", Config: []byte(`{"image":"i","host":"ssh://u@h"}`)}
	if err := sandboxes.Update(ctx, id("sb-1"), renamed, 1, false); err != nil {
		t.Fatal(err)
	}
	if got, _ = sandboxes.Get(ctx, id("sb-1")); got.Revision != 2 || got.RuntimeGen != 1 {
		t.Fatalf("after rename (revision, gen) = (%d, %d), want (2, 1)", got.Revision, got.RuntimeGen)
	}

	// A content write (credential rotation): both move.
	up := &SandboxConfig{Name: "renamed", Type: "docker", Config: []byte(`{"image":"i","host":"ssh://u@h","ssh_password":"rotated"}`)}
	if err := sandboxes.Update(ctx, id("sb-1"), up, 2, true); err != nil {
		t.Fatal(err)
	}
	if got, _ = sandboxes.Get(ctx, id("sb-1")); got.Revision != 3 || got.RuntimeGen != 2 {
		t.Fatalf("after rotation (revision, gen) = (%d, %d), want (3, 2)", got.Revision, got.RuntimeGen)
	}

	// A writer still holding revision 2 must not overwrite the rotation.
	stale := &SandboxConfig{Name: "renamed", Type: "docker", Config: []byte(`{"image":"i","host":"ssh://u@h"}`)}
	if err := sandboxes.Update(ctx, id("sb-1"), stale, 2, true); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale update: err=%v, want ErrRevisionConflict", err)
	}
	if _, err := sandboxes.Get(ctx, id("sb-1")); err != nil {
		t.Fatal(err)
	}
	if got, _ = sandboxes.Get(ctx, id("sb-1")); got.Revision != 3 {
		t.Fatalf("stale update moved the row to revision %d", got.Revision)
	}

	// An identity update bumps both (identity is content by definition).
	moved := &SandboxConfig{Name: "renamed", Type: "docker", Config: []byte(`{"image":"i","host":"ssh://u@h2"}`)}
	if s, p, err := sandboxes.UpdateIdentityIfUnreferenced(ctx, id("sb-1"), moved, 3); err != nil || s != 0 || p != 0 {
		t.Fatalf("identity update: refs=(%d,%d) err=%v", s, p, err)
	}
	if got, _ = sandboxes.Get(ctx, id("sb-1")); got.Revision != 4 || got.RuntimeGen != 3 {
		t.Fatalf("after identity update (revision, gen) = (%d, %d), want (4, 3)", got.Revision, got.RuntimeGen)
	}

	// Missing config on the plain path is still not-found, not conflict.
	if err := sandboxes.Update(ctx, NewID(), renamed, 1, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing config: err=%v, want ErrNotFound", err)
	}
}
