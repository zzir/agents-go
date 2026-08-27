package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/sandboxes"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// A target and a template of different types are refused where they are
// paired — the project write and the health check — and the refusal names
// both rows. Neither may reach a backend: a docker-only call on an E2B target
// answered "unknown sandbox target type: e2b", which is a bug report, not an
// explanation.
func TestSandboxTypesMustMatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	db := testdb.New(t)
	mgr := sandboxes.NewManager()
	targets, templates := store.NewSandboxTargetStore(db), store.NewSandboxTemplateStore(db)

	dockerTpl := &store.SandboxTemplate{ID: store.NewID(), Name: "docker-tpl", Type: "docker", Config: json.RawMessage(`{"image":"i"}`)}
	if err := templates.Create(ctx, dockerTpl); err != nil {
		t.Fatal(err)
	}
	e2bTarget := &store.SandboxTarget{ID: store.NewID(), Name: "cloud", Type: "e2b", Config: json.RawMessage(`{"api_key":"k"}`)}
	if err := targets.Create(ctx, e2bTarget); err != nil {
		t.Fatal(err)
	}

	projects := store.NewProjectStore(db)
	ph := NewProjectHandler(projects, targets, templates, mgr,
		NewTerminalHandler(targets, templates, projects, mgr, settings.NewReader(nil)), settings.NewReader(nil))
	e := newTestEngine()
	e.POST("/projects", ph.Create)
	e.POST("/sandbox-targets/:id/test", testTargetHandler(db, mgr).Test)

	for _, tc := range []struct{ name, method, path, body string }{
		{"project create", http.MethodPost, "/projects", `{"name":"p","target_id":"` + e2bTarget.ID + `","template_id":"` + dockerTpl.ID + `"}`},
		{"health check", http.MethodPost, "/sandbox-targets/" + e2bTarget.ID + "/test?template_id=" + dockerTpl.ID, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := doJSON(t, e, tc.method, tc.path, tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
			}
			body := w.Body.String()
			if !strings.Contains(body, "docker-tpl") || !strings.Contains(body, "cloud") {
				t.Errorf("the refusal names neither row: %s", body)
			}
			if strings.Contains(body, "unknown sandbox target type") {
				t.Errorf("a docker-only path ran on an e2b target: %s", body)
			}
		})
	}
}
