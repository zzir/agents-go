package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// A rebuild notes itself on the session it was asked from only when that
// session is the caller's, bound to the project and at rest. The container
// call itself needs a daemon, so the note step is driven on its own.
func TestRebuildAnnotatesRequestingSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	db := testdb.New(t)
	sessions := store.NewSessionStore(db)
	if _, err := store.NewUserStore(db).EnsureLocalUser(ctx); err != nil {
		t.Fatal(err)
	}
	tg := &store.Sandbox{ID: store.NewID(), Name: "host", Type: "docker", Config: []byte(`{"image":"i"}`)}
	if err := store.NewSandboxStore(db).Create(ctx, tg); err != nil {
		t.Fatal(err)
	}
	projects := store.NewProjectStore(db)
	proj := &store.Project{OwnerID: store.LocalUserID, SandboxID: tg.ID, Name: "p"}
	other := &store.Project{OwnerID: store.LocalUserID, SandboxID: tg.ID, Name: "q"}
	for _, p := range []*store.Project{proj, other} {
		if err := projects.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	mine := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "mine", ProjectID: proj.ID}
	elsewhere := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "elsewhere", ProjectID: other.ID}
	stranger := store.NewID()
	if _, err := db.NewInsert().Model(&store.User{ID: stranger, Email: "stranger@example.com", Role: store.RoleMember}).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	foreign := &store.Session{OwnerID: stranger, ID: store.NewID(), Name: "theirs", ProjectID: proj.ID}
	for _, s := range []*store.Session{mine, elsewhere, foreign} {
		if err := sessions.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	entries := store.NewSharedEntryStore(db)
	noteCount := func(id string) int {
		ref, err := entries.RefFor(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := store.NewEntryStoreFor(db, ref).Entries(ctx, session.Cursor{})
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, e := range rows {
			if e.Display != nil && e.Display.Kind == store.DisplayContainerRebuilt {
				n++
			}
		}
		return n
	}
	handlerWith := func(fence RunStopper) *ProjectHandler {
		h := &ProjectHandler{Notes: &RebuildNoteDeps{Sessions: sessions, Entries: entries, Fence: fence}}
		return h
	}
	call := func(h *ProjectHandler, sessionID string) {
		engine := newTestEngine()
		engine.POST("/x", func(c *gin.Context) { h.noteRebuild(c, proj, sessionID); c.Status(http.StatusNoContent) })
		doJSON(t, engine, http.MethodPost, "/x", "")
	}

	// A session of another project or another owner: no note.
	call(handlerWith(noopStopper{}), elsewhere.ID)
	call(handlerWith(noopStopper{}), foreign.ID)
	// A live run on it: no note, and no failure.
	call(handlerWith(busyStopper{}), mine.ID)
	for _, s := range []*store.Session{mine, elsewhere, foreign} {
		if n := noteCount(s.ID); n != 0 {
			t.Fatalf("session %s holds %d rebuilt notes before any should be written", s.Name, n)
		}
	}

	// The caller's own, bound and at rest: one note, for a person.
	call(handlerWith(noopStopper{}), mine.ID)
	if n := noteCount(mine.ID); n != 1 {
		t.Fatalf("session holds %d rebuilt notes, want 1", n)
	}
	ref, _ := entries.RefFor(ctx, mine.ID)
	rows, _ := store.NewEntryStoreFor(db, ref).Entries(ctx, session.Cursor{})
	note := rows[len(rows)-1]
	if note.Kind != session.EntryKindAnnotation || note.Source.Type != "host" || note.Display.Text == "" {
		t.Fatalf("note = kind %q source %q text %q; want a host annotation with text", note.Kind, note.Source.Type, note.Display.Text)
	}
	if note.Display.Extra["project_id"] != proj.ID {
		t.Fatalf("note extra = %v, want project_id %s", note.Display.Extra, proj.ID)
	}
}
