package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// statusRig mounts the session and approval routes over a runner that derives
// statuses from the real stores.
func statusRig(t *testing.T) rig {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db := testdb.New(t)
	sessions := store.NewSessionStore(db)
	tasks, approvals := store.NewTaskStore(db), store.NewPendingApprovalStore(db)
	runner := bridge.NewRunner(t.Context(), db, &bridge.AgentDeps{
		AgentConfigs: store.NewAgentConfigStore(db), Providers: store.NewProviderStore(db), Sessions: sessions,
		Traces: store.NewTraceStore(db), Settings: settings.NewReader(store.NewSettingStore(db)),
		Memories: store.NewMemoryStore(db), PendingApprovals: approvals, Tasks: tasks, Wakeups: store.NewWakeupStore(db),
	})
	s := server.New(slog.New(slog.DiscardHandler), usersByToken, nil)
	s.RegisterAPI(Handlers{
		Authz:     AuthzDeps{Sessions: sessions, Tasks: tasks, Approvals: approvals, Triggers: store.NewTriggerStore(db), Hub: runner.Hub()},
		Sessions:  NewSessionHandler(testSessionDeps(db, func(d *SessionDeps) { d.Sessions, d.Stopper, d.Statuses = sessions, runner, runner })),
		Runs:      NewRunHandler(runner),
		Agents:    testAgentConfigHandler(db),
		Tasks:     NewTaskHandler(tasks, runner),
		Approvals: NewApprovalHandler(approvals, runner),
		Triggers:  NewTriggerHandler(store.NewTriggerStore(db), sessions, store.NewWorkflowStore(db), store.NewAgentConfigStore(db), &fakeFirer{}),
		Workflows: NewWorkflowHandler(store.NewWorkflowStore(db), store.NewAgentConfigStore(db), sessions, runner),
		Skills:    NewSkillHandler(store.NewSkillStore(db), settings.NewReader(store.NewSettingStore(db))),
	}.Register)
	return rig{engine: s.Engine, sessions: sessions, db: db, runner: runner}
}

// pauseOn files an approval on sessionID, as a run paused there would.
func pauseOn(t *testing.T, r rig, sessionID, callID string) {
	t.Helper()
	calls, _ := json.Marshal([]store.PendingToolCall{{ToolCallID: callID, ToolName: "exec_command", Arguments: `{"cmd":"make"}`}})
	if err := store.NewPendingApprovalStore(r.db).Save(context.Background(), &store.PendingApproval{
		RunID: store.NewID(), SessionID: sessionID, State: "{}", ToolCalls: calls,
	}); err != nil {
		t.Fatal(err)
	}
}

// GET /approvals is the caller's own inbox: it lists what the caller's
// conversations and their tasks wait on, and nothing of anyone else's — the
// admin included, who manages and never reads.
func TestApprovalsInboxIsOwnerScoped(t *testing.T) {
	r := statusRig(t)
	ctx := context.Background()
	mine := &store.Session{OwnerID: memberUser.ID, ID: store.NewID(), Name: "mine"}
	theirs := &store.Session{OwnerID: otherUser.ID, ID: store.NewID(), Name: "theirs"}
	child := &store.Session{OwnerID: memberUser.ID, ID: store.NewID(), Name: "task", Hidden: true}
	for _, s := range []*store.Session{mine, theirs, child} {
		if err := r.sessions.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	task := &store.Task{ID: store.NewID(), RunID: store.NewID(), ParentSessionID: mine.ID, ChildSessionID: child.ID, Label: "audit", Status: protocol.TaskInputRequired}
	if err := store.NewTaskStore(r.db).Create(ctx, task); err != nil {
		t.Fatal(err)
	}
	pauseOn(t, r, mine.ID, "call_mine")
	pauseOn(t, r, child.ID, "call_task")
	pauseOn(t, r, theirs.ID, "call_theirs")

	inbox := func(u protocol.UserInfo) []bridge.PendingCall {
		t.Helper()
		rec := serve(r.engine, as(u, http.MethodGet, "/api/v1/approvals", ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /approvals as %s = %d %s", u.Email, rec.Code, rec.Body.String())
		}
		var out []bridge.PendingCall
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	got := inbox(memberUser)
	if len(got) != 2 {
		t.Fatalf("member inbox = %+v, want the conversation's call and its task's", got)
	}
	byCall := map[string]bridge.PendingCall{}
	for _, c := range got {
		byCall[c.ToolCallID] = c
	}
	if c := byCall["call_mine"]; c.SessionID != mine.ID || c.TaskID != "" {
		t.Errorf("own call = %+v, want it on the conversation itself", c)
	}
	// A task's call opens through the parent conversation, never the hidden transcript.
	if c := byCall["call_task"]; c.SessionID != mine.ID || c.TaskID != task.ID || c.TaskLabel != "audit" {
		t.Errorf("task call = %+v, want it on the parent conversation with its task", c)
	}
	if got := inbox(otherUser); len(got) != 1 || got[0].ToolCallID != "call_theirs" {
		t.Errorf("other inbox = %+v, want their one call", got)
	}
	// An empty inbox is a list, and all=true widens nothing.
	rec := serve(r.engine, as(adminUser, http.MethodGet, "/api/v1/approvals?all=true", ""))
	if rec.Code != http.StatusOK || rec.Body.String() != "[]" {
		t.Errorf("admin inbox = %d %s, want an empty list", rec.Code, rec.Body.String())
	}
}

// GET /sessions and GET /sessions/:id carry the derived status, so a tab that
// never opened a conversation still shows what it waits on.
func TestSessionsCarryDerivedStatus(t *testing.T) {
	r := statusRig(t)
	ctx := context.Background()
	waiting := &store.Session{OwnerID: memberUser.ID, ID: store.NewID(), Name: "waiting"}
	quiet := &store.Session{OwnerID: memberUser.ID, ID: store.NewID(), Name: "quiet"}
	for _, s := range []*store.Session{waiting, quiet} {
		if err := r.sessions.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	pauseOn(t, r, waiting.ID, "call_1")

	rec := serve(r.engine, as(memberUser, http.MethodGet, "/api/v1/sessions", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}
	var list []struct {
		ID              string  `json:"id"`
		Status          string  `json:"status"`
		PendingCount    int     `json:"pending_count"`
		OldestPendingAt *string `json:"oldest_pending_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, s := range list {
		seen[s.ID] = s.Status
		if s.ID == waiting.ID && (s.PendingCount != 1 || s.OldestPendingAt == nil) {
			t.Errorf("waiting row = %+v, want one pending call with its age", s)
		}
	}
	if seen[waiting.ID] != protocol.SessionRequiresAction || seen[quiet.ID] != protocol.SessionIdle {
		t.Errorf("statuses = %v, want requires_action and idle", seen)
	}

	rec = serve(r.engine, as(memberUser, http.MethodGet, "/api/v1/sessions/"+waiting.ID, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d %s", rec.Code, rec.Body.String())
	}
	var detail struct {
		Name    string               `json:"name"`
		Status  string               `json:"status"`
		Pending []bridge.PendingCall `json:"pending"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Name != "waiting" || detail.Status != protocol.SessionRequiresAction || len(detail.Pending) != 1 || detail.Pending[0].ToolCallID != "call_1" {
		t.Errorf("detail = %+v, want the session with its one pending call", detail)
	}

	// A live run of the conversation's own is named; a session with nothing
	// pending says so with an empty list, not null.
	srv := slowModel(t, time.Second)
	t.Cleanup(srv.Close)
	pv := &store.Provider{Name: "endpoint", APIKey: "k", BaseURL: srv.URL, OwnerID: memberUser.ID}
	if err := store.NewProviderStore(r.db).Create(ctx, pv); err != nil {
		t.Fatal(err)
	}
	ac := &store.AgentConfig{Name: "a", Model: "gpt-test", ProviderID: pv.ID, OwnerID: memberUser.ID}
	if err := store.NewAgentConfigStore(r.db).Create(ctx, ac); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	runID, err := r.runner.StartRun(quiet.ID, ac.ID, "", bridge.TextInput("hi"), nil, func(*bridge.RunOutcome) { close(done) })
	if err != nil {
		t.Fatal(err)
	}
	rec = serve(r.engine, as(memberUser, http.MethodGet, "/api/v1/sessions/"+quiet.ID, ""))
	var live struct {
		Status    string          `json:"status"`
		LiveRunID string          `json:"live_run_id"`
		Pending   json.RawMessage `json:"pending"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &live); err != nil {
		t.Fatal(err)
	}
	if live.Status != protocol.SessionRunning || live.LiveRunID != runID || string(live.Pending) != "[]" {
		t.Errorf("live detail = %+v, want running on %s with an empty pending list", live, runID)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the run never finished")
	}
}
