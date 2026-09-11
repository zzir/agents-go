package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// A store the agent lookup cannot read is a fault (500), not a missing agent
// (400); a name past the cap is refused at bind on create and rename alike.
func TestSessionCreateFaultsAndNameCap(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testdb.New(t)
	sh := NewSessionHandler(testSessionDeps(db))
	engine := newTestEngine()
	engine.POST("/sessions", sh.Create)
	engine.PATCH("/sessions/:id", sh.Patch)

	long := strings.Repeat("n", maxNameLen+1)
	if w := doJSON(t, engine, http.MethodPost, "/sessions", `{"name":"`+long+`"}`); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "longer than") {
		t.Fatalf("create with a long name = %d %s, want 400", w.Code, w.Body.String())
	}
	w := doJSON(t, engine, http.MethodPost, "/sessions", `{"name":"`+strings.Repeat("n", maxNameLen)+`"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create at the cap = %d %s, want 201", w.Code, w.Body.String())
	}
	id := strings.SplitN(strings.SplitN(w.Body.String(), `"id":"`, 2)[1], `"`, 2)[0]
	if w := doJSON(t, engine, http.MethodPatch, "/sessions/"+id, `{"name":"`+long+`"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("rename to a long name = %d %s, want 400", w.Code, w.Body.String())
	}
	if w := doJSON(t, engine, http.MethodPost, "/sessions", `{"agent_config_id":"`+store.NewID()+`"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("create with an unknown agent = %d %s, want 400", w.Code, w.Body.String())
	}
	_ = db.Close()
	if w := doJSON(t, engine, http.MethodPost, "/sessions", `{"agent_config_id":"`+store.NewID()+`"}`); w.Code != http.StatusInternalServerError {
		t.Fatalf("create with the store down = %d %s, want 500", w.Code, w.Body.String())
	}
}
