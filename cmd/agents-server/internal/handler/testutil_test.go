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
	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// newTestDB opens an isolated in-memory SQLite database with the full schema
// created, mirroring the store package's test helper. Each call gets its own
// database (unique shared-cache name), closed via t.Cleanup.
func newTestDB(t *testing.T) *bun.DB {
	t.Helper()
	db, err := store.NewSQLiteDB("file:" + store.NewID() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.CreateSchema(context.Background(), db); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return db
}

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
		return protocol.UserInfo{ID: "local", Email: "local@localhost", Role: "admin"}, nil
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
		MCP: noLister{}, MCPServers: store.NewMcpServerStore(db),
		Stopper: noopStopper{}, Compactor: noopCompactor{},
	}
	for _, f := range tune {
		f(&d)
	}
	return d
}

// testAgentConfigHandler wires an AgentConfigHandler over db with every store real.
func testAgentConfigHandler(db *bun.DB) *AgentConfigHandler {
	return NewAgentConfigHandler(store.NewAgentConfigStore(db), store.NewMcpServerStore(db), store.NewProviderStore(db),
		bridge.NewGuardrailResolver(store.NewGuardrailStore(db)))
}

// testSandboxHandler wires a SandboxHandler over the given store and manager,
// with a terminal registry over the same pair and the workspace given. Local
// sandboxes stay refused, as the flag defaults.
func testSandboxHandler(sandboxes *store.SandboxStore, manager *bridge.SandboxManager, workspace string) *SandboxHandler {
	return NewSandboxHandler(sandboxes, manager, false, NewTerminalHandler(sandboxes, manager, settings.NewReader(nil)), workspace)
}
