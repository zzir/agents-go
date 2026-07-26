package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// TestRestContract locks the REST API conventions: 404 for writes against
// missing resources, 204 for deletes, PATCH partial updates returning the
// resource, and the {"error": {code, message}} envelope.
func TestRestContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestDB(t)
	sessions := store.NewSessionStore(db)
	sh := NewSessionHandler(sessions, store.NewEntryStore(db, ""), store.NewTraceStore(db), store.NewAgentConfigStore(db))
	ah := NewAgentConfigHandler(store.NewAgentConfigStore(db))

	engine := gin.New()
	engine.POST("/sessions", sh.Create)
	engine.PATCH("/sessions/:id", sh.Patch)
	engine.DELETE("/sessions/:id", sh.Delete)
	engine.POST("/agents", ah.Create)
	engine.PUT("/agents/:id", ah.Update)
	engine.DELETE("/agents/:id", ah.Delete)

	// Writes against a missing resource are 404 with the error envelope.
	w := doJSON(t, engine, http.MethodPut, "/agents/nope", `{"name":"x","model":"gpt-4o"}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("PUT missing agent: got %d, want 404 (body %s)", w.Code, w.Body.String())
	}
	var envelope struct {
		Error APIError `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil || envelope.Error.Code != CodeNotFound {
		t.Errorf("error envelope wrong: %s", w.Body.String())
	}
	if w := doJSON(t, engine, http.MethodDelete, "/agents/nope", ""); w.Code != http.StatusNotFound {
		t.Errorf("DELETE missing agent: got %d, want 404", w.Code)
	}

	// Create a session, PATCH name+pin, delete with 204.
	w = doJSON(t, engine, http.MethodPost, "/sessions", `{"name":"s1"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create session: %d", w.Code)
	}
	var sess struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &sess)

	w = doJSON(t, engine, http.MethodPatch, "/sessions/"+sess.ID, `{"pinned":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("patch session: %d %s", w.Code, w.Body.String())
	}
	var patched struct {
		Name   string `json:"name"`
		Pinned bool   `json:"pinned"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &patched)
	if !patched.Pinned || patched.Name != "s1" {
		t.Errorf("patch should pin and keep name, got %+v", patched)
	}

	// PATCH with empty name is a validation error.
	if w := doJSON(t, engine, http.MethodPatch, "/sessions/"+sess.ID, `{"name":""}`); w.Code != http.StatusBadRequest {
		t.Errorf("patch empty name: got %d, want 400", w.Code)
	}

	if w := doJSON(t, engine, http.MethodDelete, "/sessions/"+sess.ID, ""); w.Code != http.StatusNoContent {
		t.Errorf("delete session: got %d, want 204", w.Code)
	}
	if w := doJSON(t, engine, http.MethodDelete, "/sessions/"+sess.ID, ""); w.Code != http.StatusNotFound {
		t.Errorf("re-delete session: got %d, want 404", w.Code)
	}

	// Update validation parity: PUT with empty name is rejected before
	// touching the store.
	w = doJSON(t, engine, http.MethodPost, "/agents", `{"name":"a1","model":"gpt-4o"}`)
	var agent struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &agent)
	if w := doJSON(t, engine, http.MethodPut, "/agents/"+agent.ID, `{"name":""}`); w.Code != http.StatusBadRequest {
		t.Errorf("PUT empty agent name: got %d, want 400", w.Code)
	}
}
