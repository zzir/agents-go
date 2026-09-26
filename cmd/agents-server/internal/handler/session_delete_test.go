package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// trustForgettingStopper records which sessions' command trust a delete drops.
type trustForgettingStopper struct {
	noopStopper
	forgot []string
}

func (s *trustForgettingStopper) ForgetSessionTrust(id string) { s.forgot = append(s.forgot, id) }

// Deleting a session drops its exec_command trust grants — in-memory state
// that would otherwise outlive the row until restart.
func TestSessionDeleteForgetsCommandTrust(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testdb.New(t)
	sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "s"}
	if err := store.NewSessionStore(db).Create(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	stopper := &trustForgettingStopper{}
	h := NewSessionHandler(testSessionDeps(db, func(d *SessionDeps) { d.Stopper = stopper }))
	engine := newTestEngine()
	engine.DELETE("/sessions/:id", h.Delete)

	if w := doJSON(t, engine, http.MethodDelete, "/sessions/"+sess.ID, ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	if len(stopper.forgot) != 1 || stopper.forgot[0] != sess.ID {
		t.Fatalf("forgot = %v, want [%s]", stopper.forgot, sess.ID)
	}
}
