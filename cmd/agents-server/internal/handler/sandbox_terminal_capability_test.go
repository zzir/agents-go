package handler

import (
	"encoding/json"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/sandboxes"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// The list/get responses advertise which sandboxes can host a web terminal:
// only a persistent container can. The frontend gates the terminal button on
// this field, so getting it wrong hides (or worse, shows) the feature.
func TestSandboxList_TerminalCapability(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testdb.New(t)
	sandboxStore := store.NewSandboxStore(db)
	seed := []struct {
		name   string
		typ    string
		config string
		want   bool
	}{
		{"docker-persistent", "docker", `{"image":"i","persistent":true}`, true},
		{"docker-ephemeral", "docker", `{"image":"i"}`, false},
		{"remote-persistent", "docker", `{"image":"i","host":"ssh://u@h","persistent":true}`, true},
	}
	for _, s := range seed {
		cfg := &store.SandboxConfig{Name: s.name, Type: s.typ}
		if s.config != "" {
			cfg.Config = json.RawMessage(s.config)
		}
		if err := sandboxStore.Create(t.Context(), cfg); err != nil {
			t.Fatal(err)
		}
	}

	h := testSandboxHandler(sandboxStore, nil, t.TempDir())
	engine := newTestEngine()
	engine.GET("/sandboxStore", h.List)

	w := doJSON(t, engine, "GET", "/sandboxStore", "")
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

// The list responses also advertise each sandbox's default workdir and
// whether a per-session custom workdir is honored — the frontend prefills
// and gates the workdir picker on these, so getting them wrong offers an
// editor that does nothing (ephemeral) or hides one that works (persistent
// /workspace subtrees).
func TestSandboxList_WorkDirDefaults(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testdb.New(t)
	sandboxStore := store.NewSandboxStore(db)
	ws := t.TempDir()
	seed := []struct {
		name     string
		typ      string
		config   string
		wantDir  string
		editable bool
	}{
		// Docker reports the container-side execution directory — /workspace —
		// never the host mount source (that is config.host_dir). Persistent
		// containers may work in a /workspace subtree, so theirs is editable.
		{"docker-persistent", "docker", `{"image":"i","persistent":true}`, "/workspace", true},
		{"docker-ephemeral", "docker", `{"image":"i"}`, "/workspace", false},
	}
	for _, s := range seed {
		cfg := &store.SandboxConfig{Name: s.name, Type: s.typ}
		if s.config != "" {
			cfg.Config = json.RawMessage(s.config)
		}
		if err := sandboxStore.Create(t.Context(), cfg); err != nil {
			t.Fatal(err)
		}
	}

	h := testSandboxHandler(sandboxStore, nil, ws)
	engine := newTestEngine()
	engine.GET("/sandboxStore", h.List)

	w := doJSON(t, engine, "GET", "/sandboxStore", "")
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	var got []store.SandboxConfig
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	byName := map[string]store.SandboxConfig{}
	for _, cfg := range got {
		byName[cfg.Name] = cfg
	}
	for _, s := range seed {
		cfg := byName[s.name]
		if cfg.DefaultWorkDir != s.wantDir || cfg.WorkDirEditable != s.editable {
			t.Errorf("%s: (default_work_dir, editable) = (%q, %v), want (%q, %v)",
				s.name, cfg.DefaultWorkDir, cfg.WorkDirEditable, s.wantDir, s.editable)
		}
	}
}

// A referenced sandbox's identity fields are frozen: the binding promises the
// config id keeps meaning the same file system. Changing the machine (or
// type, directory, mount, container) is 409 while sessions are bound; the
// non-identity half — credentials above all, since key rotation is routine —
// keeps updating freely.
func TestSandboxUpdate_FreezesIdentityWhileReferenced(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testdb.New(t)
	sandboxStore := store.NewSandboxStore(db)
	sessions := store.NewSessionStore(db)
	manager := sandboxes.NewManager(t.TempDir())
	h := testSandboxHandler(sandboxStore, manager, t.TempDir())
	engine := newTestEngine()
	engine.PUT("/sandboxStore/:id", h.Update)

	cfg := &store.SandboxConfig{Name: "box", Type: "docker", Config: json.RawMessage(`{"image":"i","host":"ssh://u@h1","persistent":true}`)}
	if err := sandboxStore.Create(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "s"}
	if err := sessions.Create(t.Context(), sess); err != nil {
		t.Fatal(err)
	}
	if won, err := sessions.BindSandboxIfEmpty(t.Context(), sess.ID, cfg.ID, "/workspace/app", 1); err != nil || !won {
		t.Fatalf("bind: won=%v err=%v", won, err)
	}

	// Identity change (the daemon) → 409, row untouched.
	w := doJSON(t, engine, "PUT", "/sandboxStore/"+cfg.ID,
		`{"name":"box","type":"docker","config":{"image":"i","host":"ssh://u@h2","persistent":true}}`)
	if w.Code != 409 {
		t.Fatalf("identity update on a referenced sandbox: %d %s, want 409", w.Code, w.Body.String())
	}
	got, err := sandboxStore.Get(t.Context(), cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	var dc store.DockerConfig
	_ = json.Unmarshal(got.Config, &dc)
	if dc.Host != "ssh://u@h1" {
		t.Fatalf("refused update changed the row: host=%q", dc.Host)
	}

	// Non-identity change (credentials, name) → 200 even while referenced.
	w = doJSON(t, engine, "PUT", "/sandboxStore/"+cfg.ID,
		`{"name":"box-renamed","type":"docker","config":{"image":"i","host":"ssh://u@h1","persistent":true,"ssh_password":"rotated"}}`)
	if w.Code != 200 {
		t.Fatalf("credential update on a referenced sandbox: %d %s, want 200", w.Code, w.Body.String())
	}
	got, _ = sandboxStore.Get(t.Context(), cfg.ID)
	_ = json.Unmarshal(got.Config, &dc)
	if dc.SSHPassword != "rotated" || got.Name != "box-renamed" {
		t.Fatalf("non-identity update did not land: name=%q password=%q", got.Name, dc.SSHPassword)
	}

	// The session gone, identity moves freely again.
	if err := sessions.Delete(t.Context(), sess.ID); err != nil {
		t.Fatal(err)
	}
	w = doJSON(t, engine, "PUT", "/sandboxStore/"+cfg.ID,
		`{"name":"box-renamed","type":"docker","config":{"image":"i","host":"ssh://u@h2","persistent":true}}`)
	if w.Code != 200 {
		t.Fatalf("identity update after the last session left: %d %s, want 200", w.Code, w.Body.String())
	}
}
