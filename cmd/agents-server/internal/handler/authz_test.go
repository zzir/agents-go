package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/cmd/agents-server/internal/authn"
	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

var (
	adminUser  = protocol.UserInfo{ID: "u-admin", Email: "admin@example.com", Role: store.RoleAdmin}
	memberUser = protocol.UserInfo{ID: "u-member", Email: "member@example.com", Role: store.RoleMember}
	otherUser  = protocol.UserInfo{ID: "u-other", Email: "other@example.com", Role: store.RoleMember}
)

// usersByToken resolves a bearer to one of the test users: the token IS the id.
func usersByToken(_ context.Context, bearer string) (protocol.UserInfo, error) {
	for _, u := range []protocol.UserInfo{adminUser, memberUser, otherUser} {
		if u.ID == bearer {
			return u, nil
		}
	}
	return protocol.UserInfo{}, store.ErrNotFound
}

// authzRig mounts the real middleware chain and the full route table over an
// in-memory database, with three users to act as.
func authzRig(t *testing.T) (*gin.Engine, *store.SessionStore, *bun.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db := newTestDB(t)
	sessions := store.NewSessionStore(db)
	agents := store.NewAgentConfigStore(db)
	deps := &bridge.AgentDeps{AgentConfigs: agents, Sessions: sessions, Traces: store.NewTraceStore(db)}
	runner := bridge.NewRunner(t.Context(), db, deps)
	s := server.New(slog.New(slog.DiscardHandler), usersByToken, nil)
	s.RegisterAPI(Handlers{
		Authz:    AuthzDeps{Sessions: sessions, Tasks: store.NewTaskStore(db), Approvals: store.NewPendingApprovalStore(db), Triggers: store.NewTriggerStore(db), Hub: runner.Hub()},
		Sessions: NewSessionHandler(testSessionDeps(db, func(d *SessionDeps) { d.Sessions, d.Agents, d.Stopper = sessions, agents, runner })),
		Runs:     NewRunHandler(runner),
		Agents:   testAgentConfigHandler(db),
	}.Register)
	return s.Engine, sessions, db
}

func as(user protocol.UserInfo, method, path, body string) *http.Request {
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Authorization", "Bearer "+user.ID)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func serve(engine *gin.Engine, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

// Shared configuration: every member reads, only admins write.
func TestSharedConfigWritesAreAdminOnly(t *testing.T) {
	engine, _, _ := authzRig(t)
	body := `{"name":"a1","model":"gpt-5.5"}`

	if rec := serve(engine, as(memberUser, http.MethodPost, "/api/v1/agents", body)); rec.Code != http.StatusForbidden {
		t.Fatalf("member create agent = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	if rec := serve(engine, as(memberUser, http.MethodGet, "/api/v1/agents", "")); rec.Code != http.StatusOK {
		t.Fatalf("member list agents = %d, want 200", rec.Code)
	}
	if rec := serve(engine, as(adminUser, http.MethodPost, "/api/v1/agents", body)); rec.Code != http.StatusCreated {
		t.Fatalf("admin create agent = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
}

// A session's content is its owner's: a foreign session reads as absent even
// to an admin; the sidebar lists one owner's; an admin may list all and
// delete, never read.
func TestSessionsArePrivateToTheirOwner(t *testing.T) {
	engine, _, _ := authzRig(t)

	rec := serve(engine, as(memberUser, http.MethodPost, "/api/v1/sessions", `{"name":"mine"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	var sess store.Session
	_ = json.Unmarshal(rec.Body.Bytes(), &sess)
	if sess.OwnerID != memberUser.ID {
		t.Fatalf("owner = %q, want the creator", sess.OwnerID)
	}

	for _, u := range []protocol.UserInfo{otherUser, adminUser} {
		if rec := serve(engine, as(u, http.MethodGet, "/api/v1/sessions/"+sess.ID, "")); rec.Code != http.StatusNotFound {
			t.Fatalf("%s GET foreign session = %d, want 404", u.ID, rec.Code)
		}
		if rec := serve(engine, as(u, http.MethodGet, "/api/v1/sessions/"+sess.ID+"/messages", "")); rec.Code != http.StatusNotFound {
			t.Fatalf("%s GET foreign messages = %d, want 404", u.ID, rec.Code)
		}
	}
	if rec := serve(engine, as(memberUser, http.MethodGet, "/api/v1/sessions/"+sess.ID, "")); rec.Code != http.StatusOK {
		t.Fatalf("owner GET = %d", rec.Code)
	}

	// Listing: the other member sees nothing, the admin's ?all=true sees it,
	// a member's ?all=true is refused.
	rec = serve(engine, as(otherUser, http.MethodGet, "/api/v1/sessions", ""))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), sess.ID) {
		t.Fatalf("other's list = %d %s, must not include a foreign session", rec.Code, rec.Body.String())
	}
	rec = serve(engine, as(adminUser, http.MethodGet, "/api/v1/sessions?all=true", ""))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), sess.ID) {
		t.Fatalf("admin all = %d %s, want the session listed", rec.Code, rec.Body.String())
	}
	if rec := serve(engine, as(memberUser, http.MethodGet, "/api/v1/sessions?all=true", "")); rec.Code != http.StatusForbidden {
		t.Fatalf("member all = %d, want 403", rec.Code)
	}

	// Delete: another member cannot; the admin can (management).
	if rec := serve(engine, as(otherUser, http.MethodDelete, "/api/v1/sessions/"+sess.ID, "")); rec.Code != http.StatusNotFound {
		t.Fatalf("other delete = %d, want 404", rec.Code)
	}
	if rec := serve(engine, as(adminUser, http.MethodDelete, "/api/v1/sessions/"+sess.ID, "")); rec.Code != http.StatusNoContent {
		t.Fatalf("admin delete = %d, want 204 (%s)", rec.Code, rec.Body.String())
	}
}

// Reassigning a session is an admin's management act: a member is refused
// (403, the write is on shared ground, not a 404 oracle), the new owner must
// be an account, and afterwards the session — and the hidden session serving
// it — reads for the new owner and not the old.
func TestSessionReassignIsAdminManagement(t *testing.T) {
	engine, sessions, db := authzRig(t)
	ctx := context.Background()
	// The rig resolves bearers to three users that exist only as UserInfo;
	// the reassign target must be a row.
	for _, u := range []protocol.UserInfo{adminUser, memberUser, otherUser} {
		if _, err := db.NewInsert().Model(&store.User{ID: u.ID, Email: u.Email, Role: u.Role}).Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}

	rec := serve(engine, as(memberUser, http.MethodPost, "/api/v1/sessions", `{"name":"mine"}`))
	var sess store.Session
	_ = json.Unmarshal(rec.Body.Bytes(), &sess)
	child := &store.Session{ID: store.NewID(), OwnerID: memberUser.ID, Hidden: true, Name: "task"}
	if err := sessions.Create(ctx, child); err != nil {
		t.Fatal(err)
	}
	if err := store.NewTaskStore(db).Create(ctx, &store.Task{ID: store.NewID(), RunID: store.NewID(), ParentSessionID: sess.ID, ChildSessionID: child.ID, Status: "completed"}); err != nil {
		t.Fatal(err)
	}

	body := `{"user_id":"` + otherUser.ID + `"}`
	if rec := serve(engine, as(memberUser, http.MethodPut, "/api/v1/sessions/"+sess.ID+"/owner", body)); rec.Code != http.StatusForbidden {
		t.Fatalf("member reassign = %d, want 403", rec.Code)
	}
	if rec := serve(engine, as(adminUser, http.MethodPut, "/api/v1/sessions/"+sess.ID+"/owner", `{"user_id":"nobody"}`)); rec.Code != http.StatusBadRequest {
		t.Fatalf("reassign to no account = %d, want 400", rec.Code)
	}
	if rec := serve(engine, as(adminUser, http.MethodPut, "/api/v1/sessions/"+store.NewID()+"/owner", body)); rec.Code != http.StatusNotFound {
		t.Fatalf("reassign a missing session = %d, want 404", rec.Code)
	}
	if rec := serve(engine, as(adminUser, http.MethodPut, "/api/v1/sessions/"+sess.ID+"/owner", body)); rec.Code != http.StatusOK {
		t.Fatalf("admin reassign = %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := serve(engine, as(otherUser, http.MethodGet, "/api/v1/sessions/"+sess.ID, "")); rec.Code != http.StatusOK {
		t.Fatalf("new owner GET = %d, want 200", rec.Code)
	}
	if rec := serve(engine, as(memberUser, http.MethodGet, "/api/v1/sessions/"+sess.ID, "")); rec.Code != http.StatusNotFound {
		t.Fatalf("old owner GET = %d, want 404", rec.Code)
	}
	if got, _ := sessions.Get(ctx, child.ID); got.OwnerID != otherUser.ID {
		t.Fatalf("the task's hidden session stayed with %s", got.OwnerID)
	}
}

// Run events reach the owner's connections and nobody else's: a second user
// watching the bus hears nothing of a run in a session they do not own.
func TestRunEventsStayWithTheOwner(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestDB(t)
	sessions := store.NewSessionStore(db)
	sess := &store.Session{OwnerID: memberUser.ID, ID: store.NewID(), Name: "mine"}
	if err := sessions.Create(t.Context(), sess); err != nil {
		t.Fatal(err)
	}
	runner := bridge.NewRunner(t.Context(), db, &bridge.AgentDeps{
		AgentConfigs: store.NewAgentConfigStore(db), Sessions: sessions, Traces: store.NewTraceStore(db),
	})
	wsh := NewWSHandler(runner, sessions, store.NewPendingApprovalStore(db))
	engine := gin.New()
	engine.GET("/ws", server.HandleWSWithAuth(wsh.Handle, usersByToken, nil))
	srv := httptest.NewServer(engine)
	t.Cleanup(srv.Close)

	dial := func(u protocol.UserInfo) *websocket.Conn {
		conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer resp.Body.Close()
		t.Cleanup(func() { _ = conn.Close() })
		_ = conn.WriteJSON(map[string]string{"type": protocol.EventAuth, "token": u.ID})
		var ack protocol.Envelope
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if err := conn.ReadJSON(&ack); err != nil || ack.Type != protocol.EventAuthOK {
			t.Fatalf("auth ack: %v", err)
		}
		return conn
	}
	owner, stranger := dial(memberUser), dial(otherUser)

	create, _ := json.Marshal(protocol.RunCreate{SessionID: sess.ID, Input: "hello"})
	if err := owner.WriteJSON(protocol.Envelope{Type: protocol.EventRunCreate, Payload: create}); err != nil {
		t.Fatal(err)
	}
	started := readUntil(t, owner, protocol.EventRunStarted)
	var sp protocol.RunStarted
	_ = json.Unmarshal(started.Payload, &sp)

	// The stranger was never attached. A probe (subscribing to the owner's
	// run) must be the FIRST thing they hear back, refused — anything leaked
	// would have arrived ahead of it.
	sub, _ := json.Marshal(protocol.RunSubscribe{RunID: sp.RunID})
	if err := stranger.WriteJSON(protocol.Envelope{Type: protocol.EventRunSubscribe, Payload: sub}); err != nil {
		t.Fatal(err)
	}
	var first protocol.Envelope
	_ = stranger.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := stranger.ReadJSON(&first); err != nil {
		t.Fatalf("stranger probe: %v", err)
	}
	var re protocol.RunError
	_ = json.Unmarshal(first.Payload, &re)
	if first.Type != protocol.EventRunError || re.Code != protocol.CodeRunNotFound {
		t.Fatalf("stranger's first event = %s %q, want the refused subscribe (nothing leaked before it)", first.Type, re.Code)
	}

	// Nor may they start a run in that session.
	if err := stranger.WriteJSON(protocol.Envelope{Type: protocol.EventRunCreate, Payload: create}); err != nil {
		t.Fatal(err)
	}
	refused := readUntil(t, stranger, protocol.EventRunError)
	_ = json.Unmarshal(refused.Payload, &re)
	if re.Code != protocol.CodeSessionNotFound {
		t.Fatalf("stranger run.create answered %q, want session_not_found", re.Code)
	}
}

// User management is the admin's: members cannot list or change roles, an
// admin can promote a member, and no admin can demote themself.
func TestUserRoleManagement(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestDB(t)
	for _, u := range []protocol.UserInfo{adminUser, memberUser} {
		if _, err := db.NewInsert().Model(&store.User{ID: u.ID, Email: u.Email, Role: u.Role}).Exec(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	local := &store.User{ID: store.LocalUserID, Email: "local@localhost", Role: store.RoleAdmin}
	s := server.New(slog.New(slog.DiscardHandler), usersByToken, nil)
	s.RegisterAPI(Handlers{Auth: NewAuthHandler(authn.NewStatic("tok", local), nil, store.NewUserStore(db), nil)}.Register)
	engine := s.Engine

	if rec := serve(engine, as(memberUser, http.MethodGet, "/api/v1/auth/users", "")); rec.Code != http.StatusForbidden {
		t.Fatalf("member list users = %d, want 403", rec.Code)
	}
	rec := serve(engine, as(adminUser, http.MethodGet, "/api/v1/auth/users", ""))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), memberUser.Email) {
		t.Fatalf("admin list users = %d %s", rec.Code, rec.Body.String())
	}
	if rec := serve(engine, as(adminUser, http.MethodPut, "/api/v1/auth/users/"+adminUser.ID+"/role", `{"role":"member"}`)); rec.Code != http.StatusBadRequest {
		t.Fatalf("self-demotion = %d, want 400", rec.Code)
	}
	if rec := serve(engine, as(adminUser, http.MethodPut, "/api/v1/auth/users/"+memberUser.ID+"/role", `{"role":"admin"}`)); rec.Code != http.StatusNoContent {
		t.Fatalf("promote = %d %s", rec.Code, rec.Body.String())
	}
	promoted, err := store.NewUserStore(db).ByID(t.Context(), memberUser.ID)
	if err != nil || promoted.Role != store.RoleAdmin {
		t.Fatalf("after promote: %+v %v", promoted, err)
	}
}
