package handler

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/authn"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// The audit log attributes every successful mutating request to its caller,
// with the route pattern as the action and the path parameter as the
// resource; reads, failures and unauthenticated requests leave no line. The
// admin reads it back; a member may not.
func TestAuditLogRecordsMutations(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestDB(t)
	audit := store.NewAuditStore(db)
	var mu sync.Mutex
	var seen []server.AuditRecord
	record := func(ctx context.Context, r server.AuditRecord) {
		mu.Lock()
		seen = append(seen, r)
		mu.Unlock()
		_ = audit.Record(ctx, &store.AuditEvent{ActorID: r.Actor.ID, ActorEmail: r.Actor.Email, Action: r.Action, Resource: r.Resource, Detail: r.Detail})
	}
	local := &store.User{ID: store.LocalUserID, Email: "local@localhost", Role: store.RoleAdmin}
	s := server.New(slog.New(slog.DiscardHandler), usersByToken, record)
	s.RegisterAPI(Handlers{
		Auth:   NewAuthHandler(authn.NewStatic("tok", local), nil, store.NewUserStore(db), audit),
		Agents: testAgentConfigHandler(db),
	}.Register)
	engine := s.Engine

	// A read leaves nothing; a refused write leaves nothing; an admin's
	// create leaves one line naming the created resource.
	serve(engine, as(adminUser, http.MethodGet, "/api/v1/agents", ""))
	serve(engine, as(memberUser, http.MethodPost, "/api/v1/agents", `{"name":"a1","model":"m"}`))
	rec := serve(engine, as(adminUser, http.MethodPost, "/api/v1/agents", `{"name":"a1","model":"m"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	n := len(seen)
	mu.Unlock()
	if n != 1 || seen[0].Actor.ID != adminUser.ID || seen[0].Action != "POST /agents" {
		t.Fatalf("audit after create = %+v, want one line by the admin for POST /agents", seen)
	}

	// An update names the resource from the path.
	id := strings.Trim(strings.SplitN(strings.SplitN(rec.Body.String(), `"id":"`, 2)[1], `"`, 2)[0], `"`)
	if rec := serve(engine, as(adminUser, http.MethodPut, "/api/v1/agents/"+id, `{"name":"a2","model":"m"}`)); rec.Code != http.StatusOK {
		t.Fatalf("update = %d %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	last := seen[len(seen)-1]
	mu.Unlock()
	if last.Action != "PUT /agents/:id" || last.Resource != id {
		t.Fatalf("audit for update = %+v", last)
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
