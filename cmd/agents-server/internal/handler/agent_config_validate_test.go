package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

func newAgentEngine(t *testing.T) (*gin.Engine, *store.McpServerStore) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db := newTestDB(t)
	mcpStore := store.NewMcpServerStore(db)
	h := NewAgentConfigHandler(store.NewAgentConfigStore(db)).WithMcpStore(mcpStore)

	engine := gin.New()
	engine.POST("/agents", h.Create)
	engine.PUT("/agents/:id", h.Update)
	return engine, mcpStore
}

func errMessage(t *testing.T, body []byte) string {
	t.Helper()
	var envelope struct {
		Error APIError `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("response is not the error envelope: %s", body)
	}
	if envelope.Error.Code != CodeValidation {
		t.Errorf("error code = %q, want %q", envelope.Error.Code, CodeValidation)
	}
	return envelope.Error.Message
}

// use_previous_response_id cannot be combined with the server's always-on
// session persistence, so saving it is rejected with a message that names the
// field — on create and on update.
func TestAgentConfigRejectsUsePreviousResponseID(t *testing.T) {
	engine, _ := newAgentEngine(t)

	w := doJSON(t, engine, http.MethodPost, "/agents", `{"name":"a","use_previous_response_id":true}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("create: got %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	if msg := errMessage(t, w.Body.Bytes()); !strings.Contains(msg, "use_previous_response_id") {
		t.Errorf("error should name the field: %q", msg)
	}

	// A clean create then an update flipping the flag on: also rejected.
	w = doJSON(t, engine, http.MethodPost, "/agents", `{"name":"a"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: got %d: %s", w.Code, w.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	w = doJSON(t, engine, http.MethodPut, "/agents/"+created.ID, `{"name":"a","use_previous_response_id":true}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("update: got %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	if msg := errMessage(t, w.Body.Bytes()); !strings.Contains(msg, "use_previous_response_id") {
		t.Errorf("error should name the field: %q", msg)
	}
}

// Two selected MCP servers sharing a name would prefix all their tools
// identically ("<name>__"), a guaranteed run-time collision — rejected at
// save time with the offending name in the message.
func TestAgentConfigRejectsDuplicateMcpServerNames(t *testing.T) {
	engine, mcpStore := newAgentEngine(t)
	ctx := context.Background()

	s1 := &store.McpServerConfig{Name: "files", TransportType: "stdio"}
	s2 := &store.McpServerConfig{Name: "files", TransportType: "stdio"}
	if err := mcpStore.Create(ctx, s1); err != nil {
		t.Fatal(err)
	}
	if err := mcpStore.Create(ctx, s2); err != nil {
		t.Fatal(err)
	}

	body := `{"name":"a","tools":"[\"` + s1.ID + `\",\"` + s2.ID + `\"]"}`
	w := doJSON(t, engine, http.MethodPost, "/agents", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("create: got %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	if msg := errMessage(t, w.Body.Bytes()); !strings.Contains(msg, `"files"`) {
		t.Errorf("error should name the colliding server name: %q", msg)
	}

	// The same server selected twice duplicates every one of its tools.
	body = `{"name":"a","tools":"[\"` + s1.ID + `\",\"` + s1.ID + `\"]"}`
	w = doJSON(t, engine, http.MethodPost, "/agents", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("create with doubled id: got %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	if msg := errMessage(t, w.Body.Bytes()); !strings.Contains(msg, "twice") {
		t.Errorf("error should say the server is selected twice: %q", msg)
	}
}

// Distinct MCP server names, unknown ids, and malformed tools JSON must all
// still save — the validator only rejects statically certain collisions.
func TestAgentConfigAcceptsValidToolSelections(t *testing.T) {
	engine, mcpStore := newAgentEngine(t)
	ctx := context.Background()

	s1 := &store.McpServerConfig{Name: "files", TransportType: "stdio"}
	s2 := &store.McpServerConfig{Name: "search", TransportType: "stdio"}
	if err := mcpStore.Create(ctx, s1); err != nil {
		t.Fatal(err)
	}
	if err := mcpStore.Create(ctx, s2); err != nil {
		t.Fatal(err)
	}

	for name, body := range map[string]string{
		"distinct names": `{"name":"a","tools":"[\"` + s1.ID + `\",\"` + s2.ID + `\"]"}`,
		"unknown id":     `{"name":"b","tools":"[\"` + s1.ID + `\",\"gone\"]"}`,
		"malformed json": `{"name":"c","tools":"not-json"}`,
		"no tools":       `{"name":"d"}`,
	} {
		if w := doJSON(t, engine, http.MethodPost, "/agents", body); w.Code != http.StatusCreated {
			t.Errorf("%s: got %d, want 201 (body %s)", name, w.Code, w.Body.String())
		}
	}
}
