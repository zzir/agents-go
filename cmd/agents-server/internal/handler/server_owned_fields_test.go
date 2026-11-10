package handler

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/guardrails"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// A create binds the stored row's own shape, so the body can spell id and
// created_at: both are the server's, and what the client sent is dropped.
func TestCreateIgnoresClientIDAndCreatedAt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testdb.New(t)
	agents := store.NewAgentConfigStore(db)
	grStore := store.NewGuardrailStore(db)
	engine := newTestEngine()
	engine.POST("/agents", testAgentConfigHandler(db).Create)
	engine.POST("/guardrails", NewGuardrailHandler(grStore, guardrails.NewResolver(grStore)).Create)
	engine.POST("/workflows", NewWorkflowHandler(store.NewWorkflowStore(db), agents, store.NewSessionStore(db), nil).Create)

	stepAgent := &store.AgentConfig{Name: "step", Model: "m", OwnerID: store.LocalUserID}
	if err := agents.Create(t.Context(), stepAgent); err != nil {
		t.Fatal(err)
	}
	const claimed = `"id":"11111111-1111-4111-8111-111111111111","created_at":"2001-01-01T00:00:00Z"`
	for path, body := range map[string]string{
		"/agents":     `{` + claimed + `,"name":"a","model":"m"}`,
		"/guardrails": `{` + claimed + `,"name":"g","stages":["input"],"mode":"regex","config":{"pattern":"x"}}`,
		"/workflows":  `{` + claimed + `,"name":"w","description":"d","steps":[{"agent_config_id":"` + stepAgent.ID + `","prompt":"p"}]}`,
	} {
		w := doJSON(t, engine, http.MethodPost, path, body)
		if w.Code != http.StatusCreated {
			t.Fatalf("POST %s: got %d: %s", path, w.Code, w.Body.String())
		}
		var got struct {
			ID        string    `json:"id"`
			CreatedAt time.Time `json:"created_at"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.ID == "11111111-1111-4111-8111-111111111111" {
			t.Errorf("POST %s kept the client's id", path)
		}
		if time.Since(got.CreatedAt) > time.Minute {
			t.Errorf("POST %s kept the client's created_at: %s", path, got.CreatedAt)
		}
	}
}
