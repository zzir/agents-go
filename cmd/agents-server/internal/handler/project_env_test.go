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
	mgr := sandboxes.NewManager()
	targets, templates := mkSandboxRows(t, db)
	h := NewProjectHandler(projects, store.NewSandboxTargetStore(db), store.NewSandboxTemplateStore(db), mgr, NewTerminalHandler(store.NewSandboxTargetStore(db), store.NewSandboxTemplateStore(db), projects, mgr, settings.NewReader(nil)))

	p := &store.Project{ID: store.NewID(), OwnerID: store.LocalUserID, TargetID: targets, TemplateID: templates, Name: "p"}
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

// The mask round-trip: EVERY value comes back masked, a mask sent back
// unchanged keeps what is stored, and the listing carries no environment at
// all.
func TestProjectEnvMaskRoundTrip(t *testing.T) {
	e, projects, p := projectEnvFixture(t)

	rec := doJSON(t, e, http.MethodPut, "/api/v1/projects/"+p.ID, jsonBody(t, map[string]any{
		"name":        "p",
		"template_id": p.TemplateID,
		"env": []store.EnvVar{
			{Key: "TOKEN", Value: "sk-live"},
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
	if len(got.Env) != 2 {
		t.Fatalf("env = %+v, want two entries", got.Env)
	}
	// Names readable, values not — an ordinary-looking one included.
	for i, want := range []string{"TOKEN", "TZ"} {
		if got.Env[i].Key != want || got.Env[i].Value != SecretMask {
			t.Errorf("env[%d] = %+v, want %s masked", i, got.Env[i], want)
		}
	}

	// A listing must never carry an environment.
	rec = doJSON(t, e, http.MethodGet, "/api/v1/projects", "")
	if bytes.Contains(rec.Body.Bytes(), []byte("TOKEN")) || bytes.Contains(rec.Body.Bytes(), []byte("env")) {
		t.Errorf("the listing carried an environment: %s", rec.Body)
	}

	// Sending the mask straight back keeps the stored value; the neighbour is
	// edited in the same request.
	rec = doJSON(t, e, http.MethodPut, "/api/v1/projects/"+p.ID, jsonBody(t, map[string]any{
		"name":        "p",
		"template_id": p.TemplateID,
		"env": []store.EnvVar{
			{Key: "TOKEN", Value: SecretMask},
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

// An empty value is not a secret: masking it would claim a value is stored
// where none is, and the sibling maskers all leave "" alone.
func TestProjectEnvEmptyValueNotMasked(t *testing.T) {
	e, _, p := projectEnvFixture(t)
	rec := doJSON(t, e, http.MethodPut, "/api/v1/projects/"+p.ID, jsonBody(t, map[string]any{
		"name":        "p",
		"template_id": p.TemplateID,
		"env":         []store.EnvVar{{Key: "EMPTY", Value: ""}, {Key: "SET", Value: "v"}},
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body)
	}
	var got projectDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Env[0].Value != "" {
		t.Errorf("empty value came back as %q, want it left empty", got.Env[0].Value)
	}
	if got.Env[1].Value != SecretMask {
		t.Errorf("set value = %q, want masked", got.Env[1].Value)
	}
}

// A mask under a name nothing is stored under has nothing to resolve to: it
// must be refused, never stored as the sentinel.
func TestProjectEnvMaskForUnknownNameRefused(t *testing.T) {
	e, _, p := projectEnvFixture(t)
	rec := doJSON(t, e, http.MethodPut, "/api/v1/projects/"+p.ID, jsonBody(t, map[string]any{
		"name":        "p",
		"template_id": p.TemplateID,
		"env":         []store.EnvVar{{Key: "BRAND_NEW", Value: SecretMask}},
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
	body := jsonBody(t, map[string]any{"name": "p", "template_id": p.TemplateID, "env": []store.EnvVar{{Key: "A", Value: "1"}}, "revision": 1})
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
	next := jsonBody(t, map[string]any{"name": "p2", "template_id": p.TemplateID, "env": []store.EnvVar{{Key: "A", Value: "1"}}, "revision": got.Revision})
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
	mgr := sandboxes.NewManager()
	targets, templates := mkSandboxRows(t, db)
	h := NewProjectHandler(projects, store.NewSandboxTargetStore(db), store.NewSandboxTemplateStore(db), mgr, NewTerminalHandler(store.NewSandboxTargetStore(db), store.NewSandboxTemplateStore(db), projects, mgr, settings.NewReader(nil)))
	foreign := &store.Project{ID: store.NewID(), OwnerID: store.NewID(), TargetID: targets, TemplateID: templates, Name: "theirs"}
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
