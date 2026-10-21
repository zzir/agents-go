package mcp

import (
	"context"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestProtocolVersionPinned locks Options.ProtocolVersion: the pinned version
// is what the handshake speaks, and one the client cannot speak is refused
// before any dial (decisions §5.90).
func TestProtocolVersionPinned(t *testing.T) {
	ctx := context.Background()
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "t", Version: "1"}, nil)
	clientT, serverT := mcpsdk.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, serverT) }()

	server, err := NewWithTransport(ctx, "t", clientT, Options{ProtocolVersion: "2025-11-25"})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	if got := server.Session().InitializeResult().ProtocolVersion; got != "2025-11-25" {
		t.Errorf("handshake spoke %q, want the pinned 2025-11-25", got)
	}

	other, _ := mcpsdk.NewInMemoryTransports()
	_, err = NewWithTransport(ctx, "t", other, Options{ProtocolVersion: "1999-01-01"})
	if err == nil || !strings.Contains(err.Error(), "1999-01-01") {
		t.Fatalf("an unknown version connected: %v", err)
	}
}
