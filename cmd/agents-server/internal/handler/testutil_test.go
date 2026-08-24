package handler

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/guardrails"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/sandboxes"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// newTestDB opens an isolated in-memory SQLite database with the full schema
// created, mirroring the store package's test helper. Each call gets its own
// database (unique shared-cache name), closed via t.Cleanup.
// newTestEngine is gin.New() with the local admin signed in, as the auth
// middleware would have done — every handler reads the caller.
func newTestEngine() *gin.Engine {
	e := gin.New()
	e.Use(func(c *gin.Context) {
		server.SetCurrentUser(c, protocol.UserInfo{ID: store.LocalUserID, Email: "local@localhost", Role: store.RoleAdmin})
		c.Next()
	})
	return e
}

// testAuthFunc is the tests' stand-in for authn's token mode.
func testAuthFunc(tok string) server.AuthFunc {
	return func(_ context.Context, bearer string) (protocol.UserInfo, error) {
		if bearer != tok {
			return protocol.UserInfo{}, errors.New("unauthorized")
		}
		return protocol.UserInfo{ID: store.LocalUserID, Email: "local@localhost", Role: "admin"}, nil
	}
}

// doJSON performs an in-process HTTP request with an optional JSON body
// against engine and returns the recorded response.
func doJSON(t *testing.T, engine *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}

// noopStopper is a RunStopper for handler tests that never start a run.
type noopStopper struct{}

func (noopStopper) StopSessionTree(string)               {}
func (noopStopper) AbortSessionDelete(string)            {}
func (noopStopper) ReleaseSessionBinding(string, string) {}
func (noopStopper) SessionBusy(string) bool              { return false }

// noopCompactor is a SessionCompactor that finds nothing to fold.
type noopCompactor struct{}

func (noopCompactor) CompactSession(context.Context, string) (bool, int, int, error) {
	return false, 0, 0, nil
}

// noLister is an MCPToolLister for a server nothing is connected to.
type noLister struct{}

func (noLister) ListToolsFor(context.Context, string) (string, []*agents.Tool, error) {
	return "", nil, errors.New("not connected")
}

// testSessionDeps wires a SessionHandler over db with every store real and
// the runner-side seams stubbed; tune adjusts the deps before construction.
func testSessionDeps(db *bun.DB, tune ...func(*SessionDeps)) SessionDeps {
	d := SessionDeps{
		Sessions: store.NewSessionStore(db), Entries: store.NewSharedEntryStore(db), Traces: store.NewTraceStore(db),
		Agents: store.NewAgentConfigStore(db), Profiles: store.NewContextProfileStore(db),
		MCP: noLister{}, MCPServers: store.NewMcpServerStore(db), Users: store.NewUserStore(db),
		Stopper: noopStopper{}, Compactor: noopCompactor{},
	}
	for _, f := range tune {
		f(&d)
	}
	return d
}

// testAgentConfigHandler wires an AgentConfigHandler over db with every store real.
func testAgentConfigHandler(db *bun.DB) *AgentConfigHandler {
	return NewAgentConfigHandler(store.NewAgentConfigStore(db), store.NewMcpServerStore(db), store.NewProviderStore(db), store.NewSkillStore(db),
		guardrails.NewResolver(store.NewGuardrailStore(db)))
}

// testSandboxHandler wires a SandboxHandler over the given store and manager,
// with a terminal registry over the same pair and the workspace given. Local
// sandboxes stay refused, as the flag defaults.
func testSandboxHandler(sandboxStore *store.SandboxStore, manager *sandboxes.Manager, _ string) *SandboxHandler {
	return NewSandboxHandler(sandboxStore, manager, NewTerminalHandler(sandboxStore, nil, manager, settings.NewReader(nil)))
}
