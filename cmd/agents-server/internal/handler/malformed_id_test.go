package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// An :id that is not an id is 404 at the route table, whatever the entity and
// the verb — before a handler (none is wired here) or the database sees it.
func TestMalformedPathIDIsNotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := newTestEngine()
	Handlers{}.Register(engine.Group(server.APIPrefix))

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/agents/nope"},
		{http.MethodDelete, "/providers/nope"},
		{http.MethodPost, "/tasks/nope/stop"},
		{http.MethodGet, "/sessions/nope/entries"},
		{http.MethodGet, "/agents/" + strings.ToUpper(store.NewID())},
	} {
		w := doJSON(t, engine, tc.method, server.APIPrefix+tc.path, "")
		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s: got %d, want 404 (%s)", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
}

// A reference in a body that is not an id names nothing — the 400 a missing
// row gets, on both dialects, never a database error.
func TestMalformedReferenceNamesNothing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testdb.New(t)
	engine := newTestEngine()
	engine.POST("/agents", testAgentConfigHandler(db).Create)
	engine.POST("/workflows", NewWorkflowHandler(store.NewWorkflowStore(db), store.NewAgentConfigStore(db), store.NewSessionStore(db), nil).Create)

	for path, body := range map[string]string{
		"/agents":    `{"name":"a","model":"m","provider_id":"nope"}`,
		"/workflows": `{"name":"w","description":"d","steps":[{"agent_config_id":"nope","prompt":"p"}]}`,
	} {
		w := doJSON(t, engine, http.MethodPost, path, body)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "names no") {
			t.Errorf("POST %s: got %d %s, want 400 naming the missing reference", path, w.Code, w.Body.String())
		}
	}
}
