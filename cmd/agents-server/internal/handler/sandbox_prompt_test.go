package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/sandboxes"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// The per-sandbox prompt is a plain top-level field, not a credential: it
// round-trips through create, get and list exactly as written, never masked.
func TestSandboxPromptRoundTripsUnmasked(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testdb.New(t)
	h := testSandboxHandler(db, sandboxes.NewManager())
	e := newTestEngine()
	e.POST("/sandboxes", h.Create)
	e.GET("/sandboxes/:id", h.Get)
	e.GET("/sandboxes", h.List)

	const prompt = "This machine has Python 3.12 and no network. Use uv, not pip."
	rec := doJSON(t, e, http.MethodPost, "/sandboxes", jsonBody(t, map[string]any{
		"name": "box", "type": "docker",
		"config": map[string]any{"image": "img"},
		"prompt": prompt,
	}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var created store.Sandbox
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Prompt != prompt {
		t.Errorf("create response prompt = %q, want %q", created.Prompt, prompt)
	}

	rec = doJSON(t, e, http.MethodGet, "/sandboxes/"+created.ID, "")
	var got store.Sandbox
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Prompt != prompt {
		t.Errorf("get prompt = %q, want %q", got.Prompt, prompt)
	}

	rec = doJSON(t, e, http.MethodGet, "/sandboxes", "")
	var rows []store.Sandbox
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Prompt != prompt {
		t.Errorf("list prompt = %+v, want one row carrying %q", rows, prompt)
	}
}

// Editing the prompt is content, not identity, and NOT a retirement trigger:
// it must not move the runtime generation of a project on the sandbox (which
// is what severs terminals and retires containers). An image change must.
func TestSandboxPromptEditIsNotAContentChange(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testdb.New(t)
	projects := store.NewProjectStore(db)
	h := testSandboxHandler(db, sandboxes.NewManager())
	e := newTestEngine()
	e.POST("/sandboxes", h.Create)
	e.PUT("/sandboxes/:id", h.Update)

	rec := doJSON(t, e, http.MethodPost, "/sandboxes", jsonBody(t, map[string]any{
		"name": "box", "type": "docker", "config": map[string]any{"image": "img"},
	}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var sb store.Sandbox
	if err := json.Unmarshal(rec.Body.Bytes(), &sb); err != nil {
		t.Fatal(err)
	}
	proj := &store.Project{ID: store.NewID(), OwnerID: store.LocalUserID, SandboxID: sb.ID, Name: "p"}
	if err := projects.Create(context.Background(), proj); err != nil {
		t.Fatal(err)
	}
	readGen := func() int64 {
		t.Helper()
		p, err := projects.Get(context.Background(), proj.ID)
		if err != nil {
			t.Fatalf("read project: %v", err)
		}
		return p.RuntimeGen
	}
	gen0 := readGen()

	// A prompt-only edit keeps the same image: the generation stays put.
	rec = doJSON(t, e, http.MethodPut, "/sandboxes/"+sb.ID, jsonBody(t, map[string]any{
		"name": "box", "type": "docker", "config": map[string]any{"image": "img"}, "prompt": "now with a prompt",
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("prompt edit: %d %s", rec.Code, rec.Body)
	}
	if gen := readGen(); gen != gen0 {
		t.Errorf("prompt-only edit moved runtime gen %d → %d; it is content, not a retirement trigger", gen0, gen)
	}

	// An image change IS a content change: the generation advances.
	rec = doJSON(t, e, http.MethodPut, "/sandboxes/"+sb.ID, jsonBody(t, map[string]any{
		"name": "box", "type": "docker", "config": map[string]any{"image": "img2"}, "prompt": "now with a prompt",
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("image edit: %d %s", rec.Code, rec.Body)
	}
	if gen := readGen(); gen != gen0+1 {
		t.Errorf("image edit runtime gen = %d, want %d (+1)", gen, gen0+1)
	}
}
