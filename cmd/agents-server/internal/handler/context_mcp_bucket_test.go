package handler

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// deadLister is a manager with no connected servers: every ask fails with no
// name, the shape a disconnected or disabled server produces.
type deadLister struct{}

func (deadLister) ListToolsFor(context.Context, string) (string, []*agents.Tool, error) {
	return "", nil, errors.New("not connected")
}

// A bucket for a server the manager cannot ask is labelled by the server's
// CONFIGURED name from the store — never by its raw id.
func TestContextMcpBucketNamesDisconnectedServers(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	mcpStore := store.NewMcpServerStore(db)
	srv := &store.McpServerConfig{Name: "AgentKey", TransportType: "streamable_http"}
	if err := mcpStore.Create(ctx, srv); err != nil {
		t.Fatal(err)
	}

	h := NewSessionHandler(testSessionDeps(db, func(d *SessionDeps) { d.MCP, d.MCPServers = deadLister{}, mcpStore }))

	buckets := h.mcpBuckets(ctx, []string{srv.ID, "gone-id"})
	if len(buckets) != 2 {
		t.Fatalf("buckets = %d, want 2", len(buckets))
	}
	if buckets[0].Source != store.ToolSourceMCP+"AgentKey" || !buckets[0].Unavailable {
		t.Fatalf("stored server bucket = %+v, want mcp:AgentKey and unavailable", buckets[0])
	}
	// A server whose row is gone has nothing better than its id — but a row
	// that exists must never be shown as a hash.
	if !strings.HasSuffix(buckets[1].Source, "gone-id") || !buckets[1].Unavailable {
		t.Fatalf("gone server bucket = %+v", buckets[1])
	}
}
