package handler

import (
	"encoding/json"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/sandboxes"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

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
	proj := &store.Project{OwnerID: store.LocalUserID, SandboxID: cfg.ID, Name: "p"}
	if err := store.NewProjectStore(db).Create(t.Context(), proj); err != nil {
		t.Fatal(err)
	}
	if won, err := sessions.BindSandboxIfEmpty(t.Context(), sess.ID, cfg.ID, proj.ID, 1); err != nil || !won {
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
