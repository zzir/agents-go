package handler

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
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
// rig is authzRig's result: the engine and the stores and runner behind it.
type rig struct {
	engine   *gin.Engine
	sessions *store.SessionStore
	db       *bun.DB
	runner   *bridge.Runner
}

func authzRig(t *testing.T) rig {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db := testdb.New(t)
	sessions := store.NewSessionStore(db)
	agents := store.NewAgentConfigStore(db)
	deps := &bridge.AgentDeps{AgentConfigs: agents, Sessions: sessions, Traces: store.NewTraceStore(db)}
	runner := bridge.NewRunner(t.Context(), db, deps)
	tasks, approvals, triggers := store.NewTaskStore(db), store.NewPendingApprovalStore(db), store.NewTriggerStore(db)
	s := server.New(slog.New(slog.DiscardHandler), usersByToken, nil)
	s.RegisterAPI(Handlers{
		Authz:     AuthzDeps{Sessions: sessions, Tasks: tasks, Approvals: approvals, Triggers: triggers, Hub: runner.Hub()},
		Sessions:  NewSessionHandler(testSessionDeps(db, func(d *SessionDeps) { d.Sessions, d.Agents, d.Stopper = sessions, agents, runner })),
		Runs:      NewRunHandler(runner),
		Agents:    testAgentConfigHandler(db),
		Tasks:     NewTaskHandler(tasks, runner),
		Approvals: NewApprovalHandler(approvals, runner),
		Triggers:  NewTriggerHandler(triggers, sessions, store.NewWorkflowStore(db), store.NewAgentConfigStore(db), &fakeFirer{}),
		Workflows: NewWorkflowHandler(store.NewWorkflowStore(db), agents, sessions, runner),
		Skills:    NewSkillHandler(store.NewSkillStore(db), settings.NewReader(store.NewSettingStore(db))),
	}.Register)
	return rig{engine: s.Engine, sessions: sessions, db: db, runner: runner}
}

// Every :id subtree filed on a session — runs, tasks, approvals, triggers —
// answers 404 to a user who does not own that session, the same as for an
// id that does not exist; and a workflow run is into a session the caller
// owns. The admin is a stranger here too: management is not reading.
func TestSessionSubtreesAreTheOwnersAlone(t *testing.T) {
	r := authzRig(t)
	engine, sessions, db := r.engine, r.sessions, r.db
	ctx := context.Background()
	sess := &store.Session{OwnerID: memberUser.ID, ID: store.NewID(), Name: "mine"}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	child := &store.Session{OwnerID: memberUser.ID, ID: store.NewID(), Name: "task", Hidden: true}
	if err := sessions.Create(ctx, child); err != nil {
		t.Fatal(err)
	}
	task := &store.Task{ID: store.NewID(), RunID: store.NewID(), ParentSessionID: sess.ID, ChildSessionID: child.ID, Status: "completed"}
	if err := store.NewTaskStore(db).Create(ctx, task); err != nil {
		t.Fatal(err)
	}
	approvals := store.NewPendingApprovalStore(db)
	pending := &store.PendingApproval{RunID: store.NewID(), SessionID: sess.ID, State: "{}", ToolCalls: json.RawMessage(`[{"tool_call_id":"call_1","tool_name":"exec_command","arguments":"{}"}]`)}
	if err := approvals.Save(ctx, pending); err != nil {
		t.Fatal(err)
	}
	wf := &store.Workflow{Name: "build", OwnerID: memberUser.ID, Steps: store.WorkflowSteps{{Prompt: "do"}}}
	if err := store.NewWorkflowStore(db).Create(ctx, wf); err != nil {
		t.Fatal(err)
	}
	trg := &store.Trigger{WorkflowID: wf.ID, SessionID: sess.ID, Kind: store.TriggerKindWebhook, Brief: "b", Secret: store.NewTriggerSecret(), Enabled: true}
	if err := store.NewTriggerStore(db).Create(ctx, trg); err != nil {
		t.Fatal(err)
	}

	probes := []struct{ method, path string }{
		{http.MethodPost, "/api/v1/tasks/" + task.ID + "/stop"},
		{http.MethodPost, "/api/v1/tasks/" + task.ID + "/dismiss"},
		{http.MethodPost, "/api/v1/approvals/call_1/approve"},
		{http.MethodPost, "/api/v1/approvals/call_1/reject"},
		{http.MethodGet, "/api/v1/triggers/" + trg.ID},
		{http.MethodDelete, "/api/v1/triggers/" + trg.ID},
		{http.MethodPost, "/api/v1/triggers/" + trg.ID + "/fire"},
	}
	for _, u := range []protocol.UserInfo{otherUser, adminUser} {
		for _, p := range probes {
			if rec := serve(engine, as(u, p.method, p.path, "")); rec.Code != http.StatusNotFound {
				t.Errorf("%s %s %s = %d, want 404", u.ID, p.method, p.path, rec.Code)
			}
		}
		body := `{"session_id":"` + sess.ID + `","input":"go"}`
		if rec := serve(engine, as(u, http.MethodPost, "/api/v1/workflows/"+wf.ID+"/runs", body)); rec.Code != http.StatusNotFound {
			t.Errorf("%s workflow run into a foreign session = %d, want 404", u.ID, rec.Code)
		}
	}
	// The owner reaches them (whatever each then answers about its state).
	if rec := serve(engine, as(memberUser, http.MethodGet, "/api/v1/triggers/"+trg.ID, "")); rec.Code != http.StatusOK {
		t.Fatalf("owner GET trigger = %d", rec.Code)
	}
	if rec := serve(engine, as(memberUser, http.MethodPost, "/api/v1/tasks/"+task.ID+"/dismiss", "")); rec.Code == http.StatusNotFound {
		t.Fatalf("owner dismiss task = 404")
	}
	// A live run: its lookup, stream and cancel are the owner's alone. The
	// run fails on config at once (no provider), but the hub keeps its record
	// for a while — long enough to be looked up.
	done := make(chan struct{})
	runID, err := r.runner.StartRun(sess.ID, "", "", "", "hi", nil, func(*bridge.RunOutcome) { close(done) })
	if err != nil {
		t.Fatal(err)
	}
	<-done
	for _, p := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/runs/" + runID},
		{http.MethodPost, "/api/v1/runs/" + runID + "/cancel"},
	} {
		for _, u := range []protocol.UserInfo{otherUser, adminUser} {
			if rec := serve(engine, as(u, p.method, p.path, "")); rec.Code != http.StatusNotFound {
				t.Errorf("%s %s %s = %d, want 404", u.ID, p.method, p.path, rec.Code)
			}
		}
	}
	if rec := serve(engine, as(memberUser, http.MethodGet, "/api/v1/runs/"+runID, "")); rec.Code != http.StatusOK {
		t.Fatalf("owner GET run = %d", rec.Code)
	}
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

// Scoped configuration (spec §5.29): a member's create lands private and
// owned; claiming global needs the admin role; a foreign private row reads
// as absent to another member and refuses the admin's edit (management is
// delete and scope change, not authorship); host configuration (sandboxes)
// stays admin-write.
func TestScopedConfigWrites(t *testing.T) {
	engine := authzRig(t).engine
	body := `{"name":"a1","model":"gpt-5.5"}`

	rec := serve(engine, as(memberUser, http.MethodPost, "/api/v1/agents", body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("member create agent = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	var ac store.AgentConfig
	_ = json.Unmarshal(rec.Body.Bytes(), &ac)
	if ac.Scope != store.ScopePrivate || ac.OwnerID != memberUser.ID {
		t.Fatalf("member create landed as (%s, %s), want private/owned", ac.Scope, ac.OwnerID)
	}

	// Global is an explicit, admin-only claim.
	if rec := serve(engine, as(memberUser, http.MethodPost, "/api/v1/agents", `{"name":"g1","model":"m","scope":"global"}`)); rec.Code != http.StatusForbidden {
		t.Fatalf("member global create = %d, want 403", rec.Code)
	}
	rec = serve(engine, as(adminUser, http.MethodPost, "/api/v1/agents", `{"name":"g1","model":"m","scope":"global"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("admin global create = %d (%s)", rec.Code, rec.Body.String())
	}
	var global store.AgentConfig
	_ = json.Unmarshal(rec.Body.Bytes(), &global)

	// The member's private row: absent to another member, listed for its owner.
	if rec := serve(engine, as(otherUser, http.MethodGet, "/api/v1/agents/"+ac.ID, "")); rec.Code != http.StatusNotFound {
		t.Fatalf("other GET foreign private agent = %d, want 404", rec.Code)
	}
	if rec := serve(engine, as(otherUser, http.MethodGet, "/api/v1/agents", "")); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), ac.ID) {
		t.Fatalf("other's list leaks the private row: %d %s", rec.Code, rec.Body.String())
	}
	if rec := serve(engine, as(memberUser, http.MethodGet, "/api/v1/agents", "")); !strings.Contains(rec.Body.String(), ac.ID) || !strings.Contains(rec.Body.String(), global.ID) {
		t.Fatalf("owner's list must carry their own and the global row: %s", rec.Body.String())
	}

	// Writes: the owner edits their own; a non-owner member never edits a
	// global row; an admin edits a global row but not a member's private one
	// (management is delete, scope change and transfer — not authorship).
	if rec := serve(engine, as(memberUser, http.MethodPut, "/api/v1/agents/"+ac.ID, `{"name":"a1","model":"m2"}`)); rec.Code != http.StatusOK {
		t.Fatalf("owner update = %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := serve(engine, as(memberUser, http.MethodPut, "/api/v1/agents/"+global.ID, `{"name":"g1","model":"m2"}`)); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner member update global = %d, want 403", rec.Code)
	}
	if rec := serve(engine, as(adminUser, http.MethodPut, "/api/v1/agents/"+ac.ID, `{"name":"a1","model":"m3"}`)); rec.Code != http.StatusForbidden {
		t.Fatalf("admin update member's private = %d, want 403", rec.Code)
	}
	// Publishing is the admin's act; the row KEEPS its author (spec §5.29).
	if rec := serve(engine, as(memberUser, http.MethodPost, "/api/v1/agents/"+ac.ID+"/scope", `{"scope":"global"}`)); rec.Code != http.StatusForbidden {
		t.Fatalf("member promote = %d, want 403", rec.Code)
	}
	if rec := serve(engine, as(adminUser, http.MethodPost, "/api/v1/agents/"+ac.ID+"/scope", `{"scope":"global"}`)); rec.Code != http.StatusNoContent {
		t.Fatalf("admin promote = %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := serve(engine, as(otherUser, http.MethodGet, "/api/v1/agents/"+ac.ID, "")); rec.Code != http.StatusOK {
		t.Fatalf("promoted row must be readable by every member: %d", rec.Code)
	}
	var promoted store.AgentConfig
	rec = serve(engine, as(memberUser, http.MethodGet, "/api/v1/agents/"+ac.ID, ""))
	_ = json.Unmarshal(rec.Body.Bytes(), &promoted)
	if promoted.OwnerID != memberUser.ID {
		t.Fatalf("promote lost the author: owner = %q, want %q", promoted.OwnerID, memberUser.ID)
	}
	// The author still edits what they published; another member does not.
	if rec := serve(engine, as(memberUser, http.MethodPut, "/api/v1/agents/"+ac.ID, `{"name":"a1","model":"m4"}`)); rec.Code != http.StatusOK {
		t.Fatalf("author update of their published row = %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := serve(engine, as(otherUser, http.MethodPut, "/api/v1/agents/"+ac.ID, `{"name":"a1","model":"m5"}`)); rec.Code != http.StatusForbidden {
		t.Fatalf("non-author member update of a global row = %d, want 403", rec.Code)
	}
	// Unpublishing is the admin's or the author's, and returns the row to the
	// author — never to the acting admin.
	if rec := serve(engine, as(otherUser, http.MethodPost, "/api/v1/agents/"+ac.ID+"/scope", `{"scope":"private"}`)); rec.Code != http.StatusForbidden {
		t.Fatalf("non-author member demote = %d, want 403", rec.Code)
	}
	if rec := serve(engine, as(adminUser, http.MethodPost, "/api/v1/agents/"+ac.ID+"/scope", `{"scope":"private"}`)); rec.Code != http.StatusNoContent {
		t.Fatalf("admin demote = %d (%s)", rec.Code, rec.Body.String())
	}
	var demoted store.AgentConfig
	rec = serve(engine, as(memberUser, http.MethodGet, "/api/v1/agents/"+ac.ID, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("the author lost their demoted row: %d", rec.Code)
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &demoted)
	if demoted.Scope != store.ScopePrivate || demoted.OwnerID != memberUser.ID {
		t.Fatalf("demoted row = (%s, %s), want the author's private set", demoted.Scope, demoted.OwnerID)
	}
	// The author may unpublish their own row too.
	if rec := serve(engine, as(memberUser, http.MethodPost, "/api/v1/agents/"+ac.ID+"/scope", `{"scope":"global"}`)); rec.Code != http.StatusForbidden {
		t.Fatalf("member re-promote = %d, want 403", rec.Code)
	}
	_ = serve(engine, as(adminUser, http.MethodPost, "/api/v1/agents/"+ac.ID+"/scope", `{"scope":"global"}`))
	if rec := serve(engine, as(memberUser, http.MethodPost, "/api/v1/agents/"+ac.ID+"/scope", `{"scope":"private"}`)); rec.Code != http.StatusNoContent {
		t.Fatalf("author demote of their own published row = %d (%s)", rec.Code, rec.Body.String())
	}
}

// An edit authorized against one owner must not land after a transfer moved
// the row: the check is re-run inside the write transaction, so the late
// write answers 409 instead of editing under a permission that is gone — and
// a workflow's write-back cannot restore the old owner (spec §5.29).
func TestUpdateRefusedAfterATransfer(t *testing.T) {
	r := authzRig(t)
	engine, ctx := r.engine, context.Background()
	for _, u := range []protocol.UserInfo{memberUser, otherUser} {
		if _, err := r.db.NewInsert().Model(&store.User{ID: u.ID, Email: u.Email, Role: u.Role}).Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}
	agents := store.NewAgentConfigStore(r.db)
	workflows := store.NewWorkflowStore(r.db)

	ac := &store.AgentConfig{Name: "movable", Model: "m", Scope: store.ScopePrivate, OwnerID: memberUser.ID}
	if err := agents.Create(ctx, ac); err != nil {
		t.Fatal(err)
	}
	wf := &store.Workflow{Name: "movable-flow", Description: "d", Scope: store.ScopePrivate, OwnerID: memberUser.ID,
		Steps: store.WorkflowSteps{{ID: store.NewID(), Name: "one", AgentConfigID: ac.ID, Prompt: "p"}}}
	if err := workflows.Create(ctx, wf); err != nil {
		t.Fatal(err)
	}
	// The transfer lands between the caller's authorization and their write —
	// modelled by moving the rows, then issuing the edit the old owner had
	// already been allowed to make.
	if err := store.SetOwnerOf(ctx, agents.CrudStore, ac.ID, otherUser.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.SetOwnerOf(ctx, workflows.CrudStore, wf.ID, otherUser.ID); err != nil {
		t.Fatal(err)
	}
	if rec := serve(engine, as(memberUser, http.MethodPut, "/api/v1/agents/"+ac.ID, `{"name":"movable","model":"m2"}`)); rec.Code != http.StatusNotFound {
		t.Fatalf("the former owner's late agent edit = %d, want 404", rec.Code)
	}
	body := `{"name":"movable-flow","description":"d","steps":[{"id":"` + wf.Steps[0].ID + `","name":"one","agent_config_id":"` + ac.ID + `","prompt":"p2"}]}`
	if rec := serve(engine, as(memberUser, http.MethodPut, "/api/v1/workflows/"+wf.ID, body)); rec.Code != http.StatusNotFound {
		t.Fatalf("the former owner's late workflow edit = %d, want 404", rec.Code)
	}
	// The new owner still holds them, unchanged by the refused writes.
	got, err := workflows.Get(ctx, wf.ID)
	if err != nil || got.OwnerID != otherUser.ID || got.Steps[0].Prompt != "p" {
		t.Fatalf("workflow after the refused edit = (%s, %+v), want the new owner and the old steps", got.OwnerID, got.Steps)
	}

	// And the INTERLEAVING itself: a write authorized against the old pair,
	// reaching the store after the transfer, is refused inside the
	// transaction rather than landing (the outer check above cannot see a
	// transfer that lands after it ran).
	late := *got
	late.Steps[0].Prompt = "p3"
	err = workflows.Update(ctx, wf.ID, &late, ownershipGuard(store.ScopePrivate, memberUser.ID,
		func(w *store.Workflow) (string, string) { return w.Scope, w.OwnerID }, nil))
	if !errors.Is(err, store.ErrOwnershipChanged) {
		t.Fatalf("a write authorized against the former owner = %v, want ErrOwnershipChanged", err)
	}
	if after, _ := workflows.Get(ctx, wf.ID); after.Steps[0].Prompt != "p" {
		t.Fatalf("the refused late write landed: %+v", after.Steps)
	}
}

// A scope request on a row the caller may not see answers 404 whatever else
// is wrong with it: the imported/authored refusal must not tell a member that
// somebody else's private skill exists (spec §5.29 — scope is no oracle).
func TestSkillScopeIsNoExistenceOracle(t *testing.T) {
	r := authzRig(t)
	ctx := context.Background()
	skills := store.NewSkillStore(r.db)

	authored := &store.Skill{Name: "theirs", Description: "d", Content: "c",
		Scope: store.ScopePrivate, OwnerID: memberUser.ID}
	imported := &store.Skill{Name: "imported", Description: "d", Content: "c",
		Scope: store.ScopePrivate, OwnerID: memberUser.ID, SourceRepo: "https://github.com/o/r"}
	for _, sk := range []*store.Skill{authored, imported} {
		if err := skills.Create(ctx, sk); err != nil {
			t.Fatal(err)
		}
	}
	for name, id := range map[string]string{
		"unknown":          store.NewID(),
		"foreign authored": authored.ID,
		"foreign imported": imported.ID,
	} {
		rec := serve(r.engine, as(otherUser, http.MethodPost, "/api/v1/skills/"+id+"/scope", `{"scope":"global"}`))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404 (%s)", name, rec.Code, rec.Body.String())
		}
	}
}

// Transferring a scoped row is the admin's: the new owner edits it, the old
// one no longer sees it, and a member cannot transfer anything (spec §5.29).
func TestScopedConfigOwnerTransfer(t *testing.T) {
	r := authzRig(t)
	engine := r.engine
	ctx := context.Background()
	users := store.NewUserStore(r.db)
	for _, u := range []protocol.UserInfo{memberUser, otherUser} {
		if _, err := r.db.NewInsert().Model(&store.User{ID: u.ID, Email: u.Email, Role: u.Role}).Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := users.ByID(ctx, otherUser.ID); err != nil {
		t.Fatal(err)
	}

	rec := serve(engine, as(memberUser, http.MethodPost, "/api/v1/agents", `{"name":"movable","model":"m"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", rec.Code, rec.Body.String())
	}
	var ac store.AgentConfig
	_ = json.Unmarshal(rec.Body.Bytes(), &ac)

	if rec := serve(engine, as(memberUser, http.MethodPut, "/api/v1/agents/"+ac.ID+"/owner", `{"user_id":"`+otherUser.ID+`"}`)); rec.Code != http.StatusForbidden {
		t.Fatalf("member transfer = %d, want 403", rec.Code)
	}
	if rec := serve(engine, as(adminUser, http.MethodPut, "/api/v1/agents/"+ac.ID+"/owner", `{"user_id":"nobody"}`)); rec.Code != http.StatusBadRequest {
		t.Fatalf("transfer to an unknown account = %d, want 400", rec.Code)
	}
	if rec := serve(engine, as(adminUser, http.MethodPut, "/api/v1/agents/"+ac.ID+"/owner", `{"user_id":"`+otherUser.ID+`"}`)); rec.Code != http.StatusNoContent {
		t.Fatalf("admin transfer = %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := serve(engine, as(otherUser, http.MethodPut, "/api/v1/agents/"+ac.ID, `{"name":"movable","model":"m2"}`)); rec.Code != http.StatusOK {
		t.Fatalf("the new owner cannot edit their row: %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := serve(engine, as(memberUser, http.MethodGet, "/api/v1/agents/"+ac.ID, "")); rec.Code != http.StatusNotFound {
		t.Fatalf("the former owner still sees the row: %d", rec.Code)
	}
}

// A session's content is its owner's: a foreign session reads as absent even
// to an admin; the sidebar lists one owner's; an admin may list all and
// delete, never read.
func TestSessionsArePrivateToTheirOwner(t *testing.T) {
	engine := authzRig(t).engine

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
	r := authzRig(t)
	engine, sessions, db := r.engine, r.sessions, r.db
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
	db := testdb.New(t)
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
	engine.GET("/ws", server.HandleWSWithAuth(wsh.Handle, usersByToken, nil, nil))
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

	// A stranger who connects MID-RUN (the register path, which replays every
	// live run of the user) gets no replay either: again the refused probe
	// is the first thing they hear.
	late := dial(otherUser)
	if err := late.WriteJSON(protocol.Envelope{Type: protocol.EventRunSubscribe, Payload: sub}); err != nil {
		t.Fatal(err)
	}
	_ = late.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := late.ReadJSON(&first); err != nil {
		t.Fatalf("late stranger probe: %v", err)
	}
	_ = json.Unmarshal(first.Payload, &re)
	if first.Type != protocol.EventRunError || re.Code != protocol.CodeRunNotFound {
		t.Fatalf("late stranger's first event = %s %q, want the refused subscribe (no replay leaked)", first.Type, re.Code)
	}

	// The broadcast bus — a fact about the session a run stream cannot carry
	// to everyone — reaches the owner's connections only.
	fact := &protocol.Envelope{Type: protocol.EventSessionSandboxBound, Payload: json.RawMessage(`{"session_id":"` + sess.ID + `"}`)}
	wsh.registry.Broadcast(fact, "", sess.ID)
	if got := readUntil(t, owner, protocol.EventSessionSandboxBound); got.Type != protocol.EventSessionSandboxBound {
		t.Fatalf("owner did not hear the broadcast")
	}
	if err := stranger.WriteJSON(protocol.Envelope{Type: protocol.EventRunSubscribe, Payload: sub}); err != nil {
		t.Fatal(err)
	}
	_ = stranger.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := stranger.ReadJSON(&first); err != nil {
		t.Fatalf("stranger probe after broadcast: %v", err)
	}
	if first.Type != protocol.EventRunError {
		t.Fatalf("stranger heard %s after a broadcast about a foreign session", first.Type)
	}
}

// User management is the admin's: members cannot list or change accounts,
// an admin promotes a member, disables one (whose credentials then stop
// authenticating) and signs one out everywhere; no admin changes their own
// account, and the last enabled admin cannot be demoted or disabled.
func TestUserManagement(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testdb.New(t)
	for _, u := range []protocol.UserInfo{adminUser, memberUser} {
		if _, err := db.NewInsert().Model(&store.User{ID: u.ID, Email: u.Email, Role: u.Role}).Exec(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	local := &store.User{ID: store.LocalUserID, Email: "local@localhost", Role: store.RoleAdmin}
	s := server.New(slog.New(slog.DiscardHandler), usersByToken, nil)
	users, tokens := store.NewUserStore(db), store.NewAuthTokenStore(db)
	s.RegisterAPI(Handlers{Auth: NewAuthHandler(authn.NewStatic("tok", local), tokens, users, nil)}.Register)
	engine := s.Engine
	ctx := t.Context()

	if rec := serve(engine, as(memberUser, http.MethodGet, "/api/v1/auth/users", "")); rec.Code != http.StatusForbidden {
		t.Fatalf("member list users = %d, want 403", rec.Code)
	}
	rec := serve(engine, as(adminUser, http.MethodGet, "/api/v1/auth/users", ""))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), memberUser.Email) {
		t.Fatalf("admin list users = %d %s", rec.Code, rec.Body.String())
	}
	if rec := serve(engine, as(adminUser, http.MethodPatch, "/api/v1/auth/users/"+adminUser.ID, `{"role":"member"}`)); rec.Code != http.StatusBadRequest {
		t.Fatalf("self-demotion = %d, want 400", rec.Code)
	}
	if rec := serve(engine, as(adminUser, http.MethodPatch, "/api/v1/auth/users/"+store.LocalUserID, `{"disabled":true}`)); rec.Code != http.StatusBadRequest {
		t.Fatalf("disabling the local account = %d, want 400", rec.Code)
	}
	if rec := serve(engine, as(adminUser, http.MethodPatch, "/api/v1/auth/users/"+memberUser.ID, `{"role":"admin"}`)); rec.Code != http.StatusNoContent {
		t.Fatalf("promote = %d %s", rec.Code, rec.Body.String())
	}
	if promoted, err := users.ByID(ctx, memberUser.ID); err != nil || promoted.Role != store.RoleAdmin {
		t.Fatalf("after promote: %+v %v", promoted, err)
	}

	// Two admins now. The other one may be demoted and disabled; after that
	// the one left cannot be.
	pat, _, err := tokens.Mint(ctx, memberUser.ID, store.TokenKindPAT, "ci", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := tokens.Authenticate(ctx, pat); err != nil {
		t.Fatalf("the PAT must authenticate before the account is disabled: %v", err)
	}
	if rec := serve(engine, as(adminUser, http.MethodPatch, "/api/v1/auth/users/"+memberUser.ID, `{"role":"member","disabled":true}`)); rec.Code != http.StatusNoContent {
		t.Fatalf("demote+disable = %d %s", rec.Code, rec.Body.String())
	}
	if _, _, err := tokens.Authenticate(ctx, pat); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a disabled account's PAT authenticated: %v", err)
	}
	if rec := serve(engine, as(adminUser, http.MethodPatch, "/api/v1/auth/users/"+memberUser.ID, `{"disabled":false}`)); rec.Code != http.StatusNoContent {
		t.Fatalf("re-enable = %d %s", rec.Code, rec.Body.String())
	}
	if err := users.Patch(ctx, adminUser.ID, store.UserPatch{Disabled: ptr(true)}); !errors.Is(err, store.ErrLastAdmin) {
		t.Fatalf("disabling the last admin = %v, want ErrLastAdmin", err)
	}
	if err := users.Patch(ctx, adminUser.ID, store.UserPatch{Role: ptr(store.RoleMember)}); !errors.Is(err, store.ErrLastAdmin) {
		t.Fatalf("demoting the last admin = %v, want ErrLastAdmin", err)
	}

	// Sign out everywhere: every token of the account goes.
	pat2, _, _ := tokens.Mint(ctx, memberUser.ID, store.TokenKindPAT, "ci", time.Time{})
	if rec := serve(engine, as(adminUser, http.MethodDelete, "/api/v1/auth/users/"+memberUser.ID+"/tokens", "")); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke all = %d %s", rec.Code, rec.Body.String())
	}
	if _, _, err := tokens.Authenticate(ctx, pat2); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a token survived the admin's revoke-all: %v", err)
	}
}

func ptr[T any](v T) *T { return &v }

// A /scope request naming the row's current scope is refused: a flip is
// defined FROM the other scope only (spec §5.29), so a repeat is a 409 rather
// than a silent no-op the caller reads as success.
func TestSetScopeSameScopeRefused(t *testing.T) {
	engine := authzRig(t).engine

	rec := serve(engine, as(memberUser, http.MethodPost, "/api/v1/agents", `{"name":"own-ag","model":"m"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("member create = %d (%s)", rec.Code, rec.Body.String())
	}
	var ac store.AgentConfig
	_ = json.Unmarshal(rec.Body.Bytes(), &ac)

	if rec := serve(engine, as(adminUser, http.MethodPost, "/api/v1/agents/"+ac.ID+"/scope", `{"scope":"private"}`)); rec.Code != http.StatusConflict {
		t.Fatalf("private->private = %d, want 409", rec.Code)
	}
	// The row is still the member's, not the admin's.
	if rec := serve(engine, as(memberUser, http.MethodGet, "/api/v1/agents/"+ac.ID, "")); rec.Code != http.StatusOK {
		t.Fatalf("owner lost the row after a refused re-home: %d", rec.Code)
	}

	if rec := serve(engine, as(adminUser, http.MethodPost, "/api/v1/agents/"+ac.ID+"/scope", `{"scope":"global"}`)); rec.Code != http.StatusNoContent {
		t.Fatalf("promote = %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := serve(engine, as(adminUser, http.MethodPost, "/api/v1/agents/"+ac.ID+"/scope", `{"scope":"global"}`)); rec.Code != http.StatusConflict {
		t.Fatalf("global->global = %d, want 409", rec.Code)
	}
}

// Session create's agent binding follows run visibility (spec §5.29): a
// foreign private agent id answers the same 400 an unknown id gets — admin
// included, since the owner's runs would refuse it — so the binding can
// never name an agent its runs cannot build, and the answer is no
// existence oracle.
func TestSessionCreateHidesForeignPrivateAgents(t *testing.T) {
	r := authzRig(t)
	ctx := context.Background()

	private := &store.AgentConfig{Name: "theirs", Model: "m", Scope: store.ScopePrivate, OwnerID: memberUser.ID}
	if err := store.NewAgentConfigStore(r.db).Create(ctx, private); err != nil {
		t.Fatal(err)
	}

	body := `{"agent_config_id":"` + private.ID + `"}`
	if rec := serve(r.engine, as(otherUser, http.MethodPost, "/api/v1/sessions", body)); rec.Code != http.StatusBadRequest {
		t.Fatalf("other member binding a foreign private agent = %d, want 400", rec.Code)
	}
	if rec := serve(r.engine, as(adminUser, http.MethodPost, "/api/v1/sessions", body)); rec.Code != http.StatusBadRequest {
		t.Fatalf("admin binding a member's private agent = %d, want 400", rec.Code)
	}
	if rec := serve(r.engine, as(memberUser, http.MethodPost, "/api/v1/sessions", body)); rec.Code != http.StatusCreated {
		t.Fatalf("owner binding their own agent = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
}
