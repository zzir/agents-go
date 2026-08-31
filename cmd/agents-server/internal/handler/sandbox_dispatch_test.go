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

// Every sandbox row the API returns carries the type's capability flags —
// List and Get alike; the values are pinned, the frontend keys off them.
func TestSandboxRowsCarrySupports(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testdb.New(t)
	s := store.NewSandboxStore(db)
	docker := &store.Sandbox{ID: store.NewID(), Name: "box", Type: "docker", Config: json.RawMessage(`{"image":"i"}`)}
	cloud := &store.Sandbox{ID: store.NewID(), Name: "cloud", Type: "e2b", Config: json.RawMessage(`{"api_key":"k","template_id":"base"}`)}
	for _, sb := range []*store.Sandbox{docker, cloud} {
		if err := s.Create(t.Context(), sb); err != nil {
			t.Fatal(err)
		}
	}
	h := testSandboxHandler(db, sandboxes.NewManager())
	e := newTestEngine()
	e.GET("/sandboxes", h.List)
	e.GET("/sandboxes/:id", h.Get)

	want := map[string]store.SandboxSupports{
		docker.ID: {Rebuild: true},
		cloud.ID:  {},
	}
	var rows []store.Sandbox
	if err := json.Unmarshal(doJSON(t, e, http.MethodGet, "/sandboxes", "").Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(want) {
		t.Fatalf("listed %d rows, want %d", len(rows), len(want))
	}
	for _, row := range rows {
		if row.Supports != want[row.ID] {
			t.Errorf("list %s: supports = %+v, want %+v", row.Name, row.Supports, want[row.ID])
		}
	}
	for id, w := range want {
		var row store.Sandbox
		if err := json.Unmarshal(doJSON(t, e, http.MethodGet, "/sandboxes/"+id, "").Body.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		if row.Supports != w {
			t.Errorf("get %s: supports = %+v, want %+v", id, row.Supports, w)
		}
	}
}

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
