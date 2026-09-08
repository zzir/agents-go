package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// memoryRig is one server with three callers: an admin, alice and bob.
type memoryRig struct {
	t                 *testing.T
	h                 *MemoryHandler
	admin, alice, bob *gin.Engine
	aliceID, bobID    string
}

func engineAs(id string, role string, h *MemoryHandler) *gin.Engine {
	e := gin.New()
	e.Use(func(c *gin.Context) {
		server.SetCurrentUser(c, protocol.UserInfo{ID: id, Email: id + "@example.com", Role: role})
		c.Next()
	})
	e.GET("/memories", h.List)
	e.POST("/memories", h.Create)
	e.GET("/memories/:id", h.Get)
	e.PUT("/memories/:id", h.Update)
	e.DELETE("/memories/:id", h.Delete)
	e.GET("/sessions/:id/memory", h.ListSession)
	e.GET("/sessions/:id/memory/*key", h.ReadSession)
	return e
}

func newMemoryRig(t *testing.T) (*memoryRig, *store.SessionStore, *store.AgentConfigStore) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db := testdb.New(t)
	sessions := store.NewSessionStore(db)
	agents := store.NewAgentConfigStore(db)
	h := NewMemoryHandler(store.NewMemoryStore(db), sessions, agents, store.NewSharedEntryStore(db))
	r := &memoryRig{t: t, h: h, aliceID: store.NewID(), bobID: store.NewID()}
	r.admin = engineAs(store.LocalUserID, store.RoleAdmin, h)
	r.alice = engineAs(r.aliceID, store.RoleMember, h)
	r.bob = engineAs(r.bobID, store.RoleMember, h)
	return r, sessions, agents
}

func (r *memoryRig) post(e *gin.Engine, body string) (int, map[string]any) {
	r.t.Helper()
	w := doJSON(r.t, e, http.MethodPost, "/memories", body)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// The write rule per scope: global is the admin's, an agent's memory its
// editor's, a session's its owner's; refusals are 403 for a visible row and
// 404 for one the caller cannot see.
func TestMemoryWriteRuleByScope(t *testing.T) {
	r, sessions, agents := newMemoryRig(t)
	ctx := context.Background()
	private := &store.AgentConfig{Name: "alice-private", Model: "m", Scope: store.ScopePrivate, OwnerID: r.aliceID}
	shared := &store.AgentConfig{Name: "shared", Model: "m", Scope: store.ScopeGlobal, OwnerID: r.aliceID}
	for _, ac := range []*store.AgentConfig{private, shared} {
		if err := agents.Create(ctx, ac); err != nil {
			t.Fatal(err)
		}
	}
	sess := &store.Session{ID: store.NewID(), OwnerID: r.aliceID, Name: "s"}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	body := func(kind, id, key string) string {
		return `{"scope_kind":"` + kind + `","scope_id":"` + id + `","key":"` + key + `","content":"remember"}`
	}
	cases := []struct {
		name string
		by   *gin.Engine
		body string
		want int
	}{
		{"admin writes global", r.admin, body("global", "", "g"), 201},
		{"member cannot write global", r.alice, body("global", "", "g2"), 403},
		{"owner writes her private agent", r.alice, body("agent", private.ID, "k"), 201},
		{"another member cannot see it", r.bob, body("agent", private.ID, "k"), 404},
		{"admin cannot write a member's private agent", r.admin, body("agent", private.ID, "k"), 403},
		{"author writes her global agent", r.alice, body("agent", shared.ID, "k"), 201},
		{"member cannot write another's global agent", r.bob, body("agent", shared.ID, "k"), 403},
		{"admin writes a global agent", r.admin, body("agent", shared.ID, "k2"), 201},
		{"owner writes her session", r.alice, body("session", sess.ID, "notes.md"), 201},
		{"another member cannot see the session", r.bob, body("session", sess.ID, "notes.md"), 404},
		{"admin does not read sessions", r.admin, body("session", sess.ID, "notes.md"), 404},
	}
	for _, c := range cases {
		code, out := r.post(c.by, c.body)
		if code != c.want {
			t.Fatalf("%s: status %d, want %d (%v)", c.name, code, c.want, out)
		}
		if code == 201 && out["written_by"] != "user" {
			t.Fatalf("%s: written_by = %v", c.name, out["written_by"])
		}
	}

	// Listing follows agent visibility, and session rows appear for nobody.
	w := doJSON(t, r.bob, http.MethodGet, "/memories", "")
	var rows []store.Memory
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	for _, m := range rows {
		if m.ScopeKind == store.MemoryScopeSession || m.ScopeID == private.ID {
			t.Fatalf("bob sees %+v", m)
		}
	}
	if len(rows) != 3 {
		t.Fatalf("bob sees %d rows, want the global one and the shared agent's two", len(rows))
	}
	if w := doJSON(t, r.bob, http.MethodGet, "/memories?scope_kind=session", ""); w.Code != 400 {
		t.Fatalf("session listing under /memories: %d", w.Code)
	}

	// The session's own routes: list and read, then delete through /memories.
	w = doJSON(t, r.alice, http.MethodGet, "/sessions/"+sess.ID+"/memory", "")
	var infos []SessionMemoryInfo
	if err := json.Unmarshal(w.Body.Bytes(), &infos); err != nil || len(infos) != 1 || infos[0].Key != "notes.md" || infos[0].Bytes != len("remember") {
		t.Fatalf("session memory list = %s (%v)", w.Body.String(), err)
	}
	w = doJSON(t, r.alice, http.MethodGet, "/sessions/"+sess.ID+"/memory/notes.md", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"content":"remember"`) {
		t.Fatalf("session memory read = %d %s", w.Code, w.Body.String())
	}
	if w := doJSON(t, r.bob, http.MethodDelete, "/memories/"+infos[0].ID, ""); w.Code != 404 {
		t.Fatalf("bob deleting alice's session memory: %d", w.Code)
	}
	if w := doJSON(t, r.alice, http.MethodDelete, "/memories/"+infos[0].ID, ""); w.Code != 204 {
		t.Fatalf("alice deleting her session memory: %d %s", w.Code, w.Body.String())
	}
}

// Validation and identity: a bad body is 400; PUT keeps scope and key.
func TestMemoryValidationAndIdentity(t *testing.T) {
	r, _, _ := newMemoryRig(t)
	for _, bad := range []string{
		`{"scope_kind":"user","key":"k","content":"x"}`,
		`{"scope_kind":"agent","key":"k","content":"x"}`,
		`{"scope_kind":"global","scope_id":"x","key":"k","content":"x"}`,
		`{"scope_kind":"global","key":"../k","content":"x"}`,
		`{"scope_kind":"global","key":"k","content":""}`,
		`{"scope_kind":"global","key":"k","content":"` + strings.Repeat("x", 9000) + `"}`,
	} {
		if code, out := r.post(r.admin, bad); code != 400 {
			t.Fatalf("%s: status %d (%v)", bad, code, out)
		}
	}
	code, out := r.post(r.admin, `{"scope_kind":"global","key":"k","content":"v1"}`)
	if code != 201 {
		t.Fatalf("create: %d %v", code, out)
	}
	id, _ := out["id"].(string)
	if w := doJSON(t, r.admin, http.MethodPut, "/memories/"+id, `{"scope_kind":"global","key":"other","content":"v2"}`); w.Code != 400 {
		t.Fatalf("PUT with another key: %d", w.Code)
	}
	w := doJSON(t, r.admin, http.MethodPut, "/memories/"+id, `{"scope_kind":"global","key":"k","content":"v2"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"content":"v2"`) || !strings.Contains(w.Body.String(), `"id":"`+id+`"`) {
		t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
	}
	// An upsert by POST replaces too, keeping the row.
	code, out = r.post(r.admin, `{"scope_kind":"global","key":"k","content":"v3"}`)
	if code != 201 || out["id"] != id || out["content"] != "v3" {
		t.Fatalf("POST on an existing key: %d %v", code, out)
	}
}

// Deleting an agent over HTTP deletes its memory with it (invariant 64). A
// memory whose agent is already gone is listed to the admin, who alone may
// delete it; nobody edits it.
func TestAgentDeleteCascadesItsMemoryOverHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testdb.New(t)
	memories, agents := store.NewMemoryStore(db), store.NewAgentConfigStore(db)
	mh := NewMemoryHandler(memories, store.NewSessionStore(db), agents, store.NewSharedEntryStore(db))
	ah := testAgentConfigHandler(db)
	aliceID := store.NewID()
	mount := func(id, role string) *gin.Engine {
		e := engineAs(id, role, mh)
		e.DELETE("/agents/:id", ah.Delete)
		return e
	}
	admin, alice := mount(store.LocalUserID, store.RoleAdmin), mount(aliceID, store.RoleMember)
	ctx := context.Background()
	remember := func(name string) (*store.AgentConfig, string) {
		ac := &store.AgentConfig{Name: name, Model: "m", Scope: store.ScopePrivate, OwnerID: aliceID}
		if err := agents.Create(ctx, ac); err != nil {
			t.Fatal(err)
		}
		w := doJSON(t, alice, http.MethodPost, "/memories", `{"scope_kind":"agent","scope_id":"`+ac.ID+`","key":"k","content":"v"}`)
		if w.Code != 201 {
			t.Fatalf("write: %d %s", w.Code, w.Body.String())
		}
		var out struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return ac, out.ID
	}
	scopeOf := func(ac *store.AgentConfig) store.MemoryScope {
		return store.MemoryScope{Kind: store.MemoryScopeAgent, ID: ac.ID}
	}

	ac, _ := remember("a")
	if w := doJSON(t, alice, http.MethodDelete, "/agents/"+ac.ID, ""); w.Code != 204 {
		t.Fatalf("delete agent: %d %s", w.Code, w.Body.String())
	}
	if rows, _ := memories.ListScope(ctx, scopeOf(ac)); len(rows) != 0 {
		t.Fatalf("agent memory survived the delete: %d rows", len(rows))
	}

	// The agent removed underneath its memory.
	ac, id := remember("b")
	if _, err := db.NewDelete().Model((*store.AgentConfig)(nil)).Where("id = ?", ac.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if w := doJSON(t, admin, http.MethodGet, "/memories", ""); !strings.Contains(w.Body.String(), `"id":"`+id+`"`) {
		t.Fatalf("the admin's list lacks the orphan: %s", w.Body.String())
	}
	if w := doJSON(t, alice, http.MethodGet, "/memories", ""); strings.Contains(w.Body.String(), id) {
		t.Fatalf("a member lists an orphan: %s", w.Body.String())
	}
	edit := `{"scope_kind":"agent","scope_id":"` + ac.ID + `","key":"k","content":"v2"}`
	for _, c := range []struct {
		name               string
		by                 *gin.Engine
		method, path, body string
		want               int
	}{
		{"the owner cannot edit an orphan", alice, http.MethodPut, "/memories/" + id, edit, 404},
		{"nor delete it", alice, http.MethodDelete, "/memories/" + id, "", 404},
		{"the admin cannot edit it either", admin, http.MethodPut, "/memories/" + id, edit, 404},
		{"the admin deletes it", admin, http.MethodDelete, "/memories/" + id, "", 204},
	} {
		if w := doJSON(t, c.by, c.method, c.path, c.body); w.Code != c.want {
			t.Fatalf("%s: %d %s", c.name, w.Code, w.Body.String())
		}
	}
	if rows, _ := memories.ListScope(ctx, scopeOf(ac)); len(rows) != 0 {
		t.Fatalf("the orphan survived the admin's delete")
	}
}
