package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/sandboxes"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// The docker operator surface — the container listing and the stop/remove
// calls — exists on no other backend, so it refuses an E2B sandbox by NAMING
// the type. Answering "unknown sandbox type: e2b" was the bug: the type is
// perfectly well known, the operation is the thing that does not exist.
func TestDockerOnlySurfaceRefusesByName(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testdb.New(t)
	cloud := &store.Sandbox{ID: store.NewID(), Name: "cloud", Type: "e2b", Config: json.RawMessage(`{"api_key":"k","template_id":"base"}`)}
	if err := store.NewSandboxStore(db).Create(t.Context(), cloud); err != nil {
		t.Fatal(err)
	}
	h := testSandboxHandler(db, sandboxes.NewManager())
	e := newTestEngine()
	e.GET("/sandboxes/:id/containers", h.Containers)
	e.POST("/sandboxes/:id/containers/:name/stop", h.StopContainer)

	for _, path := range []string{
		"/sandboxes/" + cloud.ID + "/containers",
		"/sandboxes/" + cloud.ID + "/containers/agents-x/stop",
	} {
		method := http.MethodGet
		if strings.HasSuffix(path, "/stop") {
			method = http.MethodPost
		}
		w := doJSON(t, e, method, path, "")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400: %s", path, w.Code, w.Body.String())
		}
		body := w.Body.String()
		if !strings.Contains(body, "cloud") || !strings.Contains(body, "e2b") {
			t.Errorf("%s: the refusal names neither the sandbox nor its type: %s", path, body)
		}
		if strings.Contains(body, "unknown sandbox type") {
			t.Errorf("%s: refused as an unknown type: %s", path, body)
		}
	}
}
