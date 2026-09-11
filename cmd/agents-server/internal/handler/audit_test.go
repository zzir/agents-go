package handler

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/authn"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// The audit log attributes every successful mutating request to its caller,
// with the route pattern as the action and the path parameter as the
// resource; reads, failures and unauthenticated requests leave no line. The
// admin reads it back; a member may not.
func TestAuditLogRecordsMutations(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testdb.New(t)
	audit := store.NewAuditStore(db)
	var mu sync.Mutex
	var seen []protocol.AuditRecord
	// The store write comes FIRST: recorded(n) below gates on len(seen), so
	// appending before the write lets the test read the log back while the
	// nth row is still in flight.
	record := func(ctx context.Context, r protocol.AuditRecord) {
		_ = audit.Record(ctx, &store.AuditEvent{ActorID: r.Actor.ID, ActorEmail: r.Actor.Email, Action: r.Action, Resource: r.Resource, Detail: r.Detail})
		mu.Lock()
		seen = append(seen, r)
		mu.Unlock()
	}
	local := &store.User{ID: store.LocalUserID, Email: "local@localhost", Role: store.RoleAdmin}
	s := server.New(slog.New(slog.DiscardHandler), usersByToken, record)
	s.RegisterAPI(Handlers{
		Auth:   NewAuthHandler(authn.NewStatic("tok", local), nil, store.NewUserStore(db), audit),
		Agents: testAgentConfigHandler(db),
	}.Register)
	engine := s.Engine

	// A read leaves nothing; a refused write leaves nothing; a successful
	// create leaves one line naming the created resource — the member's own
	// (private) create included, which is the point of auditing them.
	serve(engine, as(adminUser, http.MethodGet, "/api/v1/agents", ""))
	if rec := serve(engine, as(memberUser, http.MethodPost, "/api/v1/agents", `{"name":"a1","model":"m","scope":"global"}`)); rec.Code != http.StatusForbidden {
		t.Fatalf("member global create = %d, want 403", rec.Code)
	}
	rec := serve(engine, as(adminUser, http.MethodPost, "/api/v1/agents", `{"name":"a1","model":"m"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	id := strings.Trim(strings.SplitN(strings.SplitN(rec.Body.String(), `"id":"`, 2)[1], `"`, 2)[0], `"`)
	// The line lands on its own goroutine, after the response.
	recorded := func(n int) []protocol.AuditRecord {
		deadline := time.Now().Add(2 * time.Second)
		for {
			mu.Lock()
			got := append([]protocol.AuditRecord(nil), seen...)
			mu.Unlock()
			if len(got) >= n || time.Now().After(deadline) {
				return got
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	got := recorded(1)
	if len(got) != 1 || got[0].Actor.ID != adminUser.ID || got[0].Action != "POST /agents" || got[0].Resource != id {
		t.Fatalf("audit after create = %+v, want one line by the admin for POST /agents naming %s", got, id)
	}

	// An update names the resource from the path.
	if rec := serve(engine, as(adminUser, http.MethodPut, "/api/v1/agents/"+id, `{"name":"a2","model":"m"}`)); rec.Code != http.StatusOK {
		t.Fatalf("update = %d %s", rec.Code, rec.Body.String())
	}
	got = recorded(2)
	if last := got[len(got)-1]; last.Action != "PUT /agents/:id" || last.Resource != id {
		t.Fatalf("audit for update = %+v", last)
	}
	// A scope flip says which way it went: the path alone does not.
	if rec := serve(engine, as(adminUser, http.MethodPost, "/api/v1/agents/"+id+"/scope", `{"scope":"global"}`)); rec.Code != http.StatusNoContent {
		t.Fatalf("publish = %d %s", rec.Code, rec.Body.String())
	}
	got = recorded(3)
	if last := got[len(got)-1]; last.Action != "POST /agents/:id/scope" || last.Resource != id || last.Detail != "scope=global" {
		t.Fatalf("audit for publish = %+v, want detail scope=global", last)
	}

	// The admin reads the log back newest first; a member is refused.
	rec = serve(engine, as(adminUser, http.MethodGet, "/api/v1/auth/audit?limit=10", ""))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "PUT /agents/:id") || !strings.Contains(rec.Body.String(), adminUser.Email) {
		t.Fatalf("audit list = %d %s", rec.Code, rec.Body.String())
	}
	if rec := serve(engine, as(memberUser, http.MethodGet, "/api/v1/auth/audit", "")); rec.Code != http.StatusForbidden {
		t.Fatalf("member audit list = %d, want 403", rec.Code)
	}
}

// An import and a repository-group scope flip have no path parameter to
// name their resource, so the line names the repository and says what was
// created and which way the group went.
func TestAuditLogNamesImportsAndRepoScopeFlips(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testdb.New(t)
	var mu sync.Mutex
	var seen []protocol.AuditRecord
	record := func(_ context.Context, r protocol.AuditRecord) {
		mu.Lock()
		seen = append(seen, r)
		mu.Unlock()
	}
	h := NewSkillHandler(store.NewSkillStore(db), settings.NewReader(store.NewSettingStore(db)))
	gh := fakeGitHub(t, "sha1", map[string]string{"pdf/SKILL.md": pdfSkillDoc})
	h.githubAPI, h.githubRaw = gh.URL, gh.URL
	s := server.New(slog.New(slog.DiscardHandler), usersByToken, record)
	s.RegisterAPI(Handlers{Skills: h}.Register)
	engine := s.Engine
	recorded := func(n int) []protocol.AuditRecord {
		deadline := time.Now().Add(2 * time.Second)
		for {
			mu.Lock()
			got := append([]protocol.AuditRecord(nil), seen...)
			mu.Unlock()
			if len(got) >= n || time.Now().After(deadline) {
				return got
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	if rec := serve(engine, as(adminUser, http.MethodPost, "/api/v1/skill-imports", `{"url":"https://github.com/o/r"}`)); rec.Code != http.StatusOK {
		t.Fatalf("import = %d %s", rec.Code, rec.Body.String())
	}
	got := recorded(1)
	if len(got) != 1 || got[0].Action != "POST /skill-imports" || got[0].Resource != "https://github.com/o/r" ||
		got[0].Detail != "created=1 updated=0 unchanged=0 skipped=0" {
		t.Fatalf("audit for import = %+v, want the repository and the counts", got)
	}
	if rec := serve(engine, as(adminUser, http.MethodPost, "/api/v1/skill-repos/scope", `{"repo":"https://github.com/o/r","scope":"global"}`)); rec.Code != http.StatusNoContent {
		t.Fatalf("repo publish = %d %s", rec.Code, rec.Body.String())
	}
	got = recorded(2)
	if last := got[len(got)-1]; last.Action != "POST /skill-repos/scope" || last.Resource != "https://github.com/o/r" || last.Detail != "owner="+adminUser.ID+" scope=global" {
		t.Fatalf("audit for repo publish = %+v, want the repository, its owner and the scope", last)
	}
}
