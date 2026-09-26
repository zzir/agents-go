package handler

import (
	"log/slog"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/authn"
	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// A numeric query parameter that is not a non-negative integer is a 400, never
// read as 0 — on the trace listing 0 means unbounded, the costliest read there is.
func TestNumericQueryParamsRefuseGarbage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testdb.New(t)
	runner := bridge.NewRunner(t.Context(), db, &bridge.AgentDeps{
		AgentConfigs: store.NewAgentConfigStore(db), Sessions: store.NewSessionStore(db), Traces: store.NewTraceStore(db),
	})
	local := &store.User{ID: store.LocalUserID, Email: "local@localhost", Role: store.RoleAdmin}
	s := server.New(slog.New(slog.DiscardHandler), usersByToken, nil)
	s.RegisterAPI(Handlers{
		Auth:   NewAuthHandler(authn.NewStatic("tok", local), nil, store.NewUserStore(db), store.NewAuditStore(db)),
		Tasks:  NewTaskHandler(store.NewTaskStore(db), runner),
		Traces: NewTraceHandler(store.NewTraceStore(db), settings.NewReader(store.NewSettingStore(db))),
		Runs:   NewRunHandler(runner),
	}.Register)
	direct := newTestEngine()
	direct.GET("/sessions/:id/traces", NewTraceHandler(store.NewTraceStore(db), settings.NewReader(store.NewSettingStore(db))).ListBySession)
	direct.GET("/runs/:id/events", NewRunHandler(runner).Events)

	for _, bad := range []string{"-5", "abc"} {
		for _, path := range []string{"/api/v1/auth/audit?limit=", "/api/v1/tasks?limit=", "/api/v1/tasks?offset="} {
			if rec := serve(s.Engine, as(adminUser, http.MethodGet, path+bad, "")); rec.Code != http.StatusBadRequest {
				t.Errorf("GET %s%s = %d, want 400 (%s)", path, bad, rec.Code, rec.Body.String())
			}
		}
		for _, path := range []string{"/sessions/" + store.NewID() + "/traces?limit=", "/runs/" + store.NewID() + "/events?from_seq="} {
			if w := doJSON(t, direct, http.MethodGet, path+bad, ""); w.Code != http.StatusBadRequest {
				t.Errorf("GET %s%s = %d, want 400 (%s)", path, bad, w.Code, w.Body.String())
			}
		}
	}
	// Absent stays what it was: the default page, the whole listing.
	if rec := serve(s.Engine, as(adminUser, http.MethodGet, "/api/v1/tasks", "")); rec.Code != http.StatusOK {
		t.Errorf("GET /tasks = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if w := doJSON(t, direct, http.MethodGet, "/sessions/"+store.NewID()+"/traces", ""); w.Code != http.StatusOK {
		t.Errorf("GET traces without limit = %d, want 200 (%s)", w.Code, w.Body.String())
	}
}
