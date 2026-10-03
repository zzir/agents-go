package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/mcpservers"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// The list of tools plan mode admits rides in the server's config, so it
// crosses the sealed storage with the rest and comes back as saved.
func TestMcpServerReadOnlyToolsRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testdb.New(t)
	h := NewMcpServerHandler(store.NewMcpServerStore(db), mcpservers.NewManager(t.Context(), settings.NewReader(store.NewSettingStore(db))), nil, "")
	engine := newTestEngine()
	engine.POST("/mcp-servers", h.Create)
	engine.GET("/mcp-servers/:id", h.Get)
	engine.PUT("/mcp-servers/:id", h.Update)

	w := doJSON(t, engine, http.MethodPost, "/mcp-servers", `{"name":"docs","config":{"endpoint":"http://x","headers":{"X-Key":"s3cret"},"read_only_tools":["search","fetch"]}}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		ID     string `json:"id"`
		Config struct {
			ReadOnlyTools []string `json:"read_only_tools"`
		} `json:"config"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	read := func() []string {
		w := doJSON(t, engine, http.MethodGet, "/mcp-servers/"+created.ID, "")
		if w.Code != http.StatusOK {
			t.Fatalf("get: %d %s", w.Code, w.Body.String())
		}
		var got struct {
			Config struct {
				ReadOnlyTools []string `json:"read_only_tools"`
			} `json:"config"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got.Config.ReadOnlyTools
	}
	if got := read(); len(got) != 2 || got[0] != "search" || got[1] != "fetch" {
		t.Fatalf("read_only_tools after create = %v, want [search fetch]", got)
	}
	// An update with the masked header keeps the secret and takes the new list.
	w = doJSON(t, engine, http.MethodPut, "/mcp-servers/"+created.ID, `{"name":"docs","enabled":true,"config":{"endpoint":"http://x","headers":{"X-Key":"********"},"read_only_tools":["search"]}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("update: %d %s", w.Code, w.Body.String())
	}
	if got := read(); len(got) != 1 || got[0] != "search" {
		t.Fatalf("read_only_tools after update = %v, want [search]", got)
	}
}
