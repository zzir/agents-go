package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// Guardrail config must round-trip as a JSON OBJECT (the API-wide contract for
// config blobs), and the list endpoint must carry config+blocking — the edit
// form initializes from list items, so a projection that drops them silently
// wipes the stored values on the next save. The panel once sent config as a
// stringified JSON payload and every save failed with 400.
func TestGuardrailConfigObjectRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testdb.New(t)
	grStore := store.NewGuardrailStore(db)
	h := NewGuardrailHandler(grStore, bridge.NewGuardrailResolver(grStore))
	engine := newTestEngine()
	engine.POST("/guardrails", h.Create)
	engine.GET("/guardrails", h.List)
	engine.PUT("/guardrails/:id", h.Update)

	// Stringified config (the old frontend shape) must be rejected, not stored.
	bad := `{"name":"g","stages":["input"],"mode":"regex","config":"{\"pattern\":\"x\"}"}`
	if w := doJSON(t, engine, http.MethodPost, "/guardrails", bad); w.Code != http.StatusBadRequest {
		t.Fatalf("stringified config: got %d, want 400 (body %s)", w.Code, w.Body.String())
	}

	// Object config is accepted.
	good := `{"name":"g","stages":["input"],"mode":"regex","config":{"pattern":"secret"},"blocking":true}`
	w := doJSON(t, engine, http.MethodPost, "/guardrails", good)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: got %d (body %s)", w.Code, w.Body.String())
	}

	// The list projection carries every editable field for stored entries.
	w = doJSON(t, engine, http.MethodGet, "/guardrails", "")
	if w.Code != http.StatusOK {
		t.Fatalf("list: got %d", w.Code)
	}
	var defs []bridge.GuardrailDef
	if err := json.Unmarshal(w.Body.Bytes(), &defs); err != nil {
		t.Fatal(err)
	}
	var stored *bridge.GuardrailDef
	for i := range defs {
		if defs[i].ID != "" {
			stored = &defs[i]
			break
		}
	}
	if stored == nil {
		t.Fatal("stored guardrail missing from list")
	}
	var cfg store.GuardrailConfig
	if err := json.Unmarshal(stored.Config, &cfg); err != nil || cfg.Pattern != "secret" {
		t.Fatalf("list config = %s (err %v), want pattern secret", stored.Config, err)
	}
	if !stored.Blocking {
		t.Fatal("list dropped blocking")
	}

	// Editing from the list item round-trips without losing the config.
	upd := `{"name":"g","stages":["input"],"mode":"regex","config":{"pattern":"updated"},"blocking":false}`
	w = doJSON(t, engine, http.MethodPut, "/guardrails/"+stored.ID, upd)
	if w.Code != http.StatusOK {
		t.Fatalf("update: got %d (body %s)", w.Code, w.Body.String())
	}
	var after store.Guardrail
	if err := json.Unmarshal(w.Body.Bytes(), &after); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(after.Config, &cfg); err != nil || cfg.Pattern != "updated" {
		t.Fatalf("updated config = %s, want pattern updated", after.Config)
	}
}
