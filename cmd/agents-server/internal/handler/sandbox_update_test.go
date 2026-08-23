package handler

import (
	"encoding/json"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/sandboxes"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// A rename submitted from the UI must not count as a content change, even
// though the UI round-trips every config field with explicit zeros while a
// raw-API-created config spells only the non-zero ones. The runtime
// generation is the observable: a content change bumps it, a rename must not.
func TestSandboxUpdate_UIRenameIsNotAContentChange(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testdb.New(t)
	sandboxStore := store.NewSandboxStore(db)
	cfg := &store.SandboxConfig{Name: "old", Type: "ssh",
		Config: json.RawMessage(`{"addr":"h","user":"u","work_dir":"/srv"}`)}
	if err := sandboxStore.Create(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}

	h := testSandboxHandler(sandboxStore, sandboxes.NewManager(t.TempDir()), t.TempDir())
	engine := newTestEngine()
	engine.PUT("/sandboxStore/:id", h.Update)

	// The exact shape the UI sends for a rename: full field set, explicit
	// zero values for everything the stored config omitted.
	rename := `{"name":"new","type":"ssh","config":{"addr":"h","user":"u","use_agent":false,` +
		`"key_file":"","password":"","known_hosts":"","insecure_host_key":false,"work_dir":"/srv"}}`
	if w := doJSON(t, engine, "PUT", "/sandboxStore/"+cfg.ID, rename); w.Code != 200 {
		t.Fatalf("rename status = %d: %s", w.Code, w.Body.String())
	}
	cur, err := sandboxStore.Get(t.Context(), cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Name != "new" || cur.Revision != 2 {
		t.Fatalf("after rename: name=%q revision=%d, want new/2", cur.Name, cur.Revision)
	}
	if cur.RuntimeGen != 1 {
		t.Fatalf("runtime_gen = %d after a UI rename, want 1 — representation noise counted as a content change", cur.RuntimeGen)
	}

	// Control through the same path: a real (non-identity) content change —
	// flipping use_agent — does move the generation.
	flip := `{"name":"new","type":"ssh","config":{"addr":"h","user":"u","use_agent":true,` +
		`"key_file":"","password":"","known_hosts":"","insecure_host_key":false,"work_dir":"/srv"}}`
	if w := doJSON(t, engine, "PUT", "/sandboxStore/"+cfg.ID, flip); w.Code != 200 {
		t.Fatalf("content-change status = %d: %s", w.Code, w.Body.String())
	}
	cur, err = sandboxStore.Get(t.Context(), cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Revision != 3 || cur.RuntimeGen != 2 {
		t.Fatalf("after content change: revision=%d runtime_gen=%d, want 3/2", cur.Revision, cur.RuntimeGen)
	}
}

// A PUT carrying the revision its form was loaded on is refused once another
// update moved the row: the stale tab gets 409 instead of silently
// overwriting the save it never saw. Omitting the revision keeps the legacy
// last-writer-wins behavior for raw API callers.
func TestSandboxUpdate_StaleClientRevisionConflicts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testdb.New(t)
	sandboxStore := store.NewSandboxStore(db)
	cfg := &store.SandboxConfig{Name: "old", Type: "ssh",
		Config: json.RawMessage(`{"addr":"h","user":"u","work_dir":"/srv"}`)}
	if err := sandboxStore.Create(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}

	h := testSandboxHandler(sandboxStore, sandboxes.NewManager(t.TempDir()), t.TempDir())
	engine := newTestEngine()
	engine.PUT("/sandboxStore/:id", h.Update)

	body := func(name string, revision string) string {
		return `{"name":"` + name + `","type":"ssh","config":{"addr":"h","user":"u","work_dir":"/srv"}` + revision + `}`
	}

	// Tab A saves from the form it loaded at revision 1: lands, row moves on.
	if w := doJSON(t, engine, "PUT", "/sandboxStore/"+cfg.ID, body("a", `,"revision":1`)); w.Code != 200 {
		t.Fatalf("first save status = %d: %s", w.Code, w.Body.String())
	}
	// Tab B saves from ITS revision-1 form: the row is at 2 now — refused.
	if w := doJSON(t, engine, "PUT", "/sandboxStore/"+cfg.ID, body("b", `,"revision":1`)); w.Code != 409 {
		t.Fatalf("stale save status = %d, want 409: %s", w.Code, w.Body.String())
	}
	cur, err := sandboxStore.Get(t.Context(), cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Name != "a" {
		t.Fatalf("name = %q after refused stale save, want a", cur.Name)
	}
	// No revision in the request: last-writer-wins, as before the field existed.
	if w := doJSON(t, engine, "PUT", "/sandboxStore/"+cfg.ID, body("c", "")); w.Code != 200 {
		t.Fatalf("revision-less save status = %d: %s", w.Code, w.Body.String())
	}
}
