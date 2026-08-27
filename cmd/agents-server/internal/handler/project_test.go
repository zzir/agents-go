package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/sandboxes"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// memberEngine is newTestEngine with a member (non-admin) signed in.
func memberEngine(userID string) *gin.Engine {
	e := gin.New()
	e.Use(func(c *gin.Context) {
		server.SetCurrentUser(c, protocol.UserInfo{ID: userID, Email: "member@example.com", Role: store.RoleMember})
		c.Next()
	})
	return e
}

// Projects are personal, and the admin surface is management (decisions §5.28):
// a member sees and deletes only their own rows and never a storage_hint;
// an admin lists every owner's (?all=true, hints included) and deletes any.
func TestProjectAdminSurface(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	db := testdb.New(t)
	projects := store.NewProjectStore(db)
	mgr := sandboxes.NewManager()
	targets, templates := mkSandboxRows(t, db)
	h := NewProjectHandler(projects, store.NewSandboxTargetStore(db), store.NewSandboxTemplateStore(db), mgr, NewTerminalHandler(store.NewSandboxTargetStore(db), store.NewSandboxTemplateStore(db), projects, mgr, settings.NewReader(nil)), settings.NewReader(nil))

	memberID := store.NewID()
	memberProj := &store.Project{ID: store.NewID(), OwnerID: memberID, TargetID: targets, TemplateID: templates, Name: "member-proj"}
	adminProj := &store.Project{ID: store.NewID(), OwnerID: store.LocalUserID, TargetID: targets, TemplateID: templates, Name: "admin-proj"}
	for _, p := range []*store.Project{memberProj, adminProj} {
		if err := projects.Create(ctx, p); err != nil {
			t.Fatalf("create project %s: %v", p.Name, err)
		}
	}

	mount := func(e *gin.Engine) *gin.Engine {
		e.GET("/projects", h.List)
		e.DELETE("/projects/:id", h.Delete)
		return e
	}
	member := mount(memberEngine(memberID))
	admin := mount(newTestEngine())

	decode := func(t *testing.T, body []byte) []store.Project {
		t.Helper()
		var out []store.Project
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("decode listing: %v", err)
		}
		return out
	}

	// Member: own rows only, no hint; ?all=true refused; foreign delete 404s.
	w := doJSON(t, member, http.MethodGet, "/projects", "")
	if w.Code != http.StatusOK {
		t.Fatalf("member list: %d", w.Code)
	}
	rows := decode(t, w.Body.Bytes())
	if len(rows) != 1 || rows[0].ID != memberProj.ID {
		t.Fatalf("member listing = %+v, want just their own row", rows)
	}
	if rows[0].StorageHint != "" {
		t.Fatalf("member listing carries storage_hint %q, want none", rows[0].StorageHint)
	}
	if w := doJSON(t, member, http.MethodGet, "/projects?all=true", ""); w.Code != http.StatusForbidden {
		t.Fatalf("member ?all=true: %d, want 403", w.Code)
	}
	if w := doJSON(t, member, http.MethodDelete, "/projects/"+adminProj.ID, ""); w.Code != http.StatusNotFound {
		t.Fatalf("member deleting a foreign project: %d, want 404", w.Code)
	}

	// Admin: ?all=true lists every owner's row, hints included.
	w = doJSON(t, admin, http.MethodGet, "/projects?all=true", "")
	if w.Code != http.StatusOK {
		t.Fatalf("admin list all: %d", w.Code)
	}
	rows = decode(t, w.Body.Bytes())
	if len(rows) != 2 {
		t.Fatalf("admin listing has %d rows, want 2", len(rows))
	}
	for _, r := range rows {
		if r.StorageHint == "" {
			t.Fatalf("admin listing row %s misses its storage_hint", r.Name)
		}
	}

	// Admin delete of a member's project: refused while a session binds it,
	// gone once unbound.
	sess := &store.Session{ID: store.NewID(), OwnerID: memberID, Name: "s", ProjectID: memberProj.ID}
	sessions := store.NewSessionStore(db)
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if w := doJSON(t, admin, http.MethodDelete, "/projects/"+memberProj.ID, ""); w.Code != http.StatusConflict {
		t.Fatalf("admin delete of a bound project: %d, want 409", w.Code)
	}
	if err := sessions.Delete(ctx, sess.ID); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if w := doJSON(t, admin, http.MethodDelete, "/projects/"+memberProj.ID, ""); w.Code != http.StatusNoContent {
		t.Fatalf("admin delete of a member's project: %d, want 204", w.Code)
	}
	if _, err := projects.Get(ctx, memberProj.ID); err == nil {
		t.Fatal("member project still present after the admin delete")
	}
}
