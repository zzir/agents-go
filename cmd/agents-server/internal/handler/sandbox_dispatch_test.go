package handler

import (
	"encoding/json"
	"net/http"
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
	e.GET("/sandboxes", wrap(h.List))
	e.GET("/sandboxes/:id", wrap(h.Get))

	want := map[string]store.SandboxSupports{
		docker.ID: {Rebuild: true},
		cloud.ID:  {PublicHost: true},
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
