package handler

import (
	"encoding/json"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// The list/get responses advertise which sandboxes can host a web terminal:
// ssh always, docker only when persistent, local never. The frontend gates
// the terminal button on this field, so getting it wrong hides (or worse,
// shows) the feature.
func TestSandboxList_TerminalCapability(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestDB(t)
	sandboxes := store.NewSandboxStore(db)
	seed := []struct {
		name   string
		typ    string
		config string
		want   bool
	}{
		{"ssh", "ssh", `{"addr":"h","user":"u"}`, true},
		{"docker-persistent", "docker", `{"image":"i","persistent":true}`, true},
		{"docker-ephemeral", "docker", `{"image":"i"}`, false},
		{"local", "local", ``, false},
	}
	for _, s := range seed {
		cfg := &store.SandboxConfig{Name: s.name, Type: s.typ}
		if s.config != "" {
			cfg.Config = json.RawMessage(s.config)
		}
		if err := sandboxes.Create(t.Context(), cfg); err != nil {
			t.Fatal(err)
		}
	}

	h := NewSandboxHandler(sandboxes, nil, false)
	engine := gin.New()
	engine.GET("/sandboxes", h.List)

	w := doJSON(t, engine, "GET", "/sandboxes", "")
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	var got []store.SandboxConfig
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	byName := map[string]bool{}
	for _, cfg := range got {
		byName[cfg.Name] = cfg.Terminal
	}
	for _, s := range seed {
		if byName[s.name] != s.want {
			t.Errorf("%s: terminal = %v, want %v", s.name, byName[s.name], s.want)
		}
	}
}
