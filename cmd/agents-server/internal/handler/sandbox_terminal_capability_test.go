package handler

import (
	"encoding/json"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/sandboxes"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// The list/get responses advertise which sandboxStore can host a web terminal:
// ssh always, docker only when persistent, local never. The frontend gates
// the terminal button on this field, so getting it wrong hides (or worse,
// shows) the feature.
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

// The list responses also advertise each sandbox's default workdir and whether
// a per-session custom workdir is honored — the frontend prefills and gates
// the workdir picker on these, so getting them wrong offers an editor that
// does nothing (docker) or hides one that works (local/ssh).
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
		{"ssh", "ssh", `{"addr":"h","user":"u","work_dir":"/srv/app"}`, "/srv/app", true},
		{"ssh-nodir", "ssh", `{"addr":"h","user":"u"}`, "", true},
		// Docker reports the container-side execution directory — /workspace —
		// never the host mount source (that is config.host_dir). Persistent
		// containers may work in a /workspace subtree, so theirs is editable.
		{"docker-persistent", "docker", `{"image":"i","persistent":true}`, "/workspace", true},
		{"docker-ephemeral", "docker", `{"image":"i"}`, "/workspace", false},
		{"local", "local", ``, ws, true},
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

	cfg := &store.SandboxConfig{Name: "box", Type: "ssh", Config: json.RawMessage(`{"addr":"h1","user":"u","work_dir":"/srv"}`)}
	if err := sandboxStore.Create(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "s"}
	if err := sessions.Create(t.Context(), sess); err != nil {
		t.Fatal(err)
	}
	if won, err := sessions.BindSandboxIfEmpty(t.Context(), sess.ID, cfg.ID, "/srv/app", 1); err != nil || !won {
		t.Fatalf("bind: won=%v err=%v", won, err)
	}

	// Identity change (addr) → 409, row untouched.
	w := doJSON(t, engine, "PUT", "/sandboxStore/"+cfg.ID,
		`{"name":"box","type":"ssh","config":{"addr":"h2","user":"u","work_dir":"/srv"}}`)
	if w.Code != 409 {
		t.Fatalf("identity update on a referenced sandbox: %d %s, want 409", w.Code, w.Body.String())
	}
	// The ssh USER is identity too: user-a@host and user-b@host are different
	// homes and permission views — a different file system at one address.
	w = doJSON(t, engine, "PUT", "/sandboxStore/"+cfg.ID,
		`{"name":"box","type":"ssh","config":{"addr":"h1","user":"someone-else","work_dir":"/srv"}}`)
	if w.Code != 409 {
		t.Fatalf("user change on a referenced sandbox: %d %s, want 409", w.Code, w.Body.String())
	}
	got, err := sandboxStore.Get(t.Context(), cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	var sc store.SSHConfig
	_ = json.Unmarshal(got.Config, &sc)
	if sc.Addr != "h1" || sc.User != "u" {
		t.Fatalf("refused update changed the row: addr=%q user=%q", sc.Addr, sc.User)
	}

	// Non-identity change (credentials, name) → 200 even while referenced.
	w = doJSON(t, engine, "PUT", "/sandboxStore/"+cfg.ID,
		`{"name":"box-renamed","type":"ssh","config":{"addr":"h1","user":"u","work_dir":"/srv","password":"rotated"}}`)
	if w.Code != 200 {
		t.Fatalf("credential update on a referenced sandbox: %d %s, want 200", w.Code, w.Body.String())
	}
	got, _ = sandboxStore.Get(t.Context(), cfg.ID)
	_ = json.Unmarshal(got.Config, &sc)
	if sc.Password != "rotated" || got.Name != "box-renamed" {
		t.Fatalf("non-identity update did not land: name=%q password=%q", got.Name, sc.Password)
	}

	// The session gone, identity moves freely again.
	if err := sessions.Delete(t.Context(), sess.ID); err != nil {
		t.Fatal(err)
	}
	w = doJSON(t, engine, "PUT", "/sandboxStore/"+cfg.ID,
		`{"name":"box-renamed","type":"ssh","config":{"addr":"h2","user":"u","work_dir":"/srv"}}`)
	if w.Code != 200 {
		t.Fatalf("identity update after the last session left: %d %s, want 200", w.Code, w.Body.String())
	}
}
