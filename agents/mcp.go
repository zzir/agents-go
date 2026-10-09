package agents

import "context"

// MCPServer is a connection to a Model Context Protocol server that lends its
// tools to an agent; the client lives in the mcp module — see decisions §5.7.
type MCPServer interface {
	// Name identifies the server for tracing and tool namespacing.
	Name() string
	// ListTools returns the tools the server currently exposes.
	ListTools(ctx context.Context, rc *RunContext, agent *Agent) ([]*Tool, error)
	// Close releases the server connection.
	Close() error
}
