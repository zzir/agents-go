package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// TestRestContract locks the REST API conventions: 404 for writes against
// missing resources, 204 for deletes, PATCH partial updates returning the
// resource, and the {"error": {code, message}} envelope.
func TestRestContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testdb.New(t)
	sessions := store.NewSessionStore(db)
	sh := NewSessionHandler(testSessionDeps(db, func(d *SessionDeps) { d.Sessions = sessions }))
	ah := testAgentConfigHandler(db)

	engine := newTestEngine()
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
	// Literal bytes, not a re-marshalled protocol.ErrorResponse — the wire
	// shape is the contract, and comparing the envelope against itself would
	// survive a renamed field. server/auth_test.go pins the same bytes for the
	// 401/404s that package writes itself.
	const wantEnvelope = `{"error":{"code":"not_found","message":"not found"}}`
	if got := strings.TrimSpace(w.Body.String()); got != wantEnvelope {
		t.Errorf("error envelope = %s, want %s", got, wantEnvelope)
	}
	if w := doJSON(t, engine, http.MethodDelete, "/agents/nope", ""); w.Code != http.StatusNotFound {
		t.Errorf("DELETE missing agent: got %d, want 404", w.Code)
	}

	// Create a session, PATCH name+pin, delete with 204.
	w = doJSON(t, engine, http.MethodPost, "/sessions", `{"name":"s1"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create session: %d", w.Code)
	}
	// A fresh session has no sandbox binding — the omitempty fields must be
	// absent, not empty strings, or clients would treat "" as a binding.
	if body := w.Body.String(); strings.Contains(body, "sandbox_id") || strings.Contains(body, "work_dir") {
		t.Errorf("created session leaks empty binding fields: %s", body)
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

// A fork inherits the source's sandbox binding verbatim — it continues the
// same conversation over the same file system context, with no CAS of its own.
func TestForkCopiesSandboxBinding(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testdb.New(t)
	sessions := store.NewSessionStore(db)
	// BindSandboxIfEmpty refuses a target with no config row (EXISTS).
	if err := store.NewSandboxStore(db).Create(t.Context(), &store.SandboxConfig{ID: "sb-1", Name: "sb-1", Type: "docker", Config: json.RawMessage(`{"image":"i","persistent":true}`)}); err != nil {
		t.Fatal(err)
	}
	sh := NewSessionHandler(testSessionDeps(db, func(d *SessionDeps) { d.Sessions = sessions }))

	engine := newTestEngine()
	engine.POST("/sessions", sh.Create)
	engine.POST("/sessions/:id/fork", sh.Fork)

	w := doJSON(t, engine, http.MethodPost, "/sessions", `{"name":"src"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create session: %d", w.Code)
	}
	var src struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &src)
	if err := store.NewProjectStore(db).Create(t.Context(), &store.Project{ID: "p-1", OwnerID: store.LocalUserID, SandboxID: "sb-1", Name: "p-1"}); err != nil {
		t.Fatal(err)
	}
	if won, err := sessions.BindSandboxIfEmpty(t.Context(), src.ID, "sb-1", "p-1", 1); err != nil || !won {
		t.Fatalf("bind: won=%v err=%v", won, err)
	}

	w = doJSON(t, engine, http.MethodPost, "/sessions/"+src.ID+"/fork", `{}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("fork: %d %s", w.Code, w.Body.String())
	}
	var forked struct {
		SandboxID string `json:"sandbox_id"`
		ProjectID string `json:"project_id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &forked)
	if forked.SandboxID != "sb-1" || forked.ProjectID != "p-1" {
		t.Fatalf("fork binding = (%q,%q), want the source's (sb-1,p-1)", forked.SandboxID, forked.ProjectID)
	}
}

// A session's (sandbox_id, project_id) binding is immutable: PATCH carries
// no binding fields, so a request naming one changes nothing — switching
// projects means starting (or forking into) another session.
func TestPatchCannotMoveBinding(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testdb.New(t)
	sessions := store.NewSessionStore(db)
	if err := store.NewSandboxStore(db).Create(t.Context(), &store.SandboxConfig{ID: "sb-1", Name: "sb-1", Type: "docker", Config: json.RawMessage(`{"image":"i","persistent":true}`)}); err != nil {
		t.Fatal(err)
	}
	sh := NewSessionHandler(testSessionDeps(db, func(d *SessionDeps) { d.Sessions = sessions }))

	engine := newTestEngine()
	engine.POST("/sessions", sh.Create)
	engine.PATCH("/sessions/:id", sh.Patch)

	w := doJSON(t, engine, http.MethodPost, "/sessions", `{"name":"s"}`)
	var sess struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &sess)

	if err := store.NewProjectStore(db).Create(t.Context(), &store.Project{ID: "p-1", OwnerID: store.LocalUserID, SandboxID: "sb-1", Name: "p-1"}); err != nil {
		t.Fatal(err)
	}
	if won, err := sessions.BindSandboxIfEmpty(t.Context(), sess.ID, "sb-1", "p-1", 1); err != nil || !won {
		t.Fatalf("bind: won=%v err=%v", won, err)
	}
	w = doJSON(t, engine, http.MethodPatch, "/sessions/"+sess.ID, `{"name":"renamed","project_id":"elsewhere"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", w.Code, w.Body.String())
	}
	var patched struct {
		Name      string `json:"name"`
		SandboxID string `json:"sandbox_id"`
		ProjectID string `json:"project_id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &patched)
	if patched.Name != "renamed" {
		t.Fatalf("rename lost: %q", patched.Name)
	}
	if patched.SandboxID != "sb-1" || patched.ProjectID != "p-1" {
		t.Fatalf("binding moved to (%q,%q), want (sb-1,p-1) untouched", patched.SandboxID, patched.ProjectID)
	}
}
