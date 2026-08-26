package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/sandboxes"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// projectEnvFixture stands up the project routes over a real store, with one
// sandbox and one project owned by the caller.
func projectEnvFixture(t *testing.T) (*gin.Engine, *store.ProjectStore, *store.Project) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db := testdb.New(t)
	projects := store.NewProjectStore(db)
	sbStore := store.NewSandboxStore(db)
	mgr := sandboxes.NewManager(t.TempDir())
	h := NewProjectHandler(projects, sbStore, mgr, NewTerminalHandler(sbStore, projects, mgr, settings.NewReader(nil)))

	sb := &store.SandboxConfig{ID: store.NewID(), Name: "sb", Type: "docker", Config: []byte(`{"image":"i"}`)}
	if err := sbStore.Create(context.Background(), sb); err != nil {
		t.Fatal(err)
	}
	p := &store.Project{ID: store.NewID(), OwnerID: store.LocalUserID, SandboxID: sb.ID, Name: "p"}
	if err := projects.Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	e := newTestEngine()
	g := e.Group("/api/v1/projects")
	g.GET("", h.List)
	g.GET("/:id", h.Get)
	g.PUT("/:id", h.Update)
	return e, projects, p
}

// jsonBody renders a request body for the package's doJSON helper.
func jsonBody(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The mask round-trip: a hidden value comes back masked, goes back masked,
// and survives — while the listing never carries an environment at all.
func TestProjectEnvMaskRoundTrip(t *testing.T) {
	e, projects, p := projectEnvFixture(t)

	rec := doJSON(t, e, http.MethodPut, "/api/v1/projects/"+p.ID, jsonBody(t, map[string]any{
		"name": "p",
		"env": []store.EnvVar{
			{Key: "TOKEN", Value: "sk-live", Hidden: true},
			{Key: "TZ", Value: "UTC"},
		},
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body)
	}
	var got projectDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Env) != 2 || got.Env[0].Key != "TOKEN" || got.Env[0].Value != SecretMask {
		t.Fatalf("hidden value not masked: %+v", got.Env)
	}
	if got.Env[1].Value != "UTC" {
		t.Errorf("a visible value was masked: %+v", got.Env[1])
	}

	// A listing must never carry an environment.
	rec = doJSON(t, e, http.MethodGet, "/api/v1/projects", "")
	if bytes.Contains(rec.Body.Bytes(), []byte("TOKEN")) || bytes.Contains(rec.Body.Bytes(), []byte("env")) {
		t.Errorf("the listing carried an environment: %s", rec.Body)
	}

	// Sending the mask straight back keeps the stored value.
	rec = doJSON(t, e, http.MethodPut, "/api/v1/projects/"+p.ID, jsonBody(t, map[string]any{
		"name": "p",
		"env": []store.EnvVar{
			{Key: "TOKEN", Value: SecretMask, Hidden: true},
			{Key: "TZ", Value: "Europe/Berlin"},
		},
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("second update: %d %s", rec.Code, rec.Body)
	}
	stored, err := projects.Get(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	vars, err := store.DecodeProjectEnv(stored.Env)
	if err != nil {
		t.Fatal(err)
	}
	if vars[0].Value != "sk-live" {
		t.Errorf("the masked value did not survive: %+v", vars[0])
	}
	if vars[1].Value != "Europe/Berlin" {
		t.Errorf("the edited value did not land: %+v", vars[1])
	}
}

// Unhiding a value needs no retyping — the mask resolves before the flag is
// applied — and the value then comes back readable.
func TestProjectEnvUnhideKeepsValue(t *testing.T) {
	e, _, p := projectEnvFixture(t)
	if rec := doJSON(t, e, http.MethodPut, "/api/v1/projects/"+p.ID, jsonBody(t, map[string]any{
		"name": "p",
		"env":  []store.EnvVar{{Key: "TOKEN", Value: "sk-live", Hidden: true}},
	})); rec.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body)
	}
	rec := doJSON(t, e, http.MethodPut, "/api/v1/projects/"+p.ID, jsonBody(t, map[string]any{
		"name": "p",
		"env":  []store.EnvVar{{Key: "TOKEN", Value: SecretMask}},
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("unhide: %d %s", rec.Code, rec.Body)
	}
	var got projectDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Env[0].Value != "sk-live" || got.Env[0].Hidden {
		t.Errorf("after unhiding = %+v, want the plaintext value visible", got.Env[0])
	}
}

// A mask under a name nothing is stored under has nothing to resolve to: it
// must be refused, never stored as the sentinel.
func TestProjectEnvMaskForUnknownNameRefused(t *testing.T) {
	e, _, p := projectEnvFixture(t)
	rec := doJSON(t, e, http.MethodPut, "/api/v1/projects/"+p.ID, jsonBody(t, map[string]any{
		"name": "p",
		"env":  []store.EnvVar{{Key: "BRAND_NEW", Value: SecretMask, Hidden: true}},
	}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 (%s)", rec.Code, rec.Body)
	}
}

// The update is a compare-and-set against the revision the edit was made on,
// and the response carries the revision the NEXT one must use — a stale one
// there would make every second save conflict.
func TestProjectEnvStaleRevisionConflicts(t *testing.T) {
	e, _, p := projectEnvFixture(t)
	body := jsonBody(t, map[string]any{"name": "p", "env": []store.EnvVar{{Key: "A", Value: "1"}}, "revision": 1})
	rec := doJSON(t, e, http.MethodPut, "/api/v1/projects/"+p.ID, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("first: %d %s", rec.Code, rec.Body)
	}
	var got projectDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Revision != 2 {
		t.Errorf("revision in the response = %d, want 2", got.Revision)
	}
	if rec := doJSON(t, e, http.MethodPut, "/api/v1/projects/"+p.ID, body); rec.Code != http.StatusConflict {
		t.Fatalf("stale revision: %d, want 409 (%s)", rec.Code, rec.Body)
	}
	// The revision the response handed back is the one that works.
	next := jsonBody(t, map[string]any{"name": "p2", "env": []store.EnvVar{{Key: "A", Value: "1"}}, "revision": got.Revision})
	if rec := doJSON(t, e, http.MethodPut, "/api/v1/projects/"+p.ID, next); rec.Code != http.StatusOK {
		t.Fatalf("update at the returned revision: %d, want 200 (%s)", rec.Code, rec.Body)
	}
}

// An environment is the owner's: an admin manages projects but does not read
// what they hold.
func TestProjectEnvForeignReadsAsAbsent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testdb.New(t)
	projects := store.NewProjectStore(db)
	sbStore := store.NewSandboxStore(db)
	mgr := sandboxes.NewManager(t.TempDir())
	h := NewProjectHandler(projects, sbStore, mgr, NewTerminalHandler(sbStore, projects, mgr, settings.NewReader(nil)))
	sb := &store.SandboxConfig{ID: store.NewID(), Name: "sb", Type: "docker", Config: []byte(`{"image":"i"}`)}
	if err := sbStore.Create(context.Background(), sb); err != nil {
		t.Fatal(err)
	}
	foreign := &store.Project{ID: store.NewID(), OwnerID: store.NewID(), SandboxID: sb.ID, Name: "theirs"}
	if err := projects.Create(context.Background(), foreign); err != nil {
		t.Fatal(err)
	}
	e := newTestEngine() // the test caller is the local admin
	e.GET("/api/v1/projects/:id", h.Get)

	rec := doJSON(t, e, http.MethodGet, "/api/v1/projects/"+foreign.ID, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("admin reading a member's environment: %d, want 404 (%s)", rec.Code, rec.Body)
	}
}
