package agents

import "context"

// MCPServer is a connection to a Model Context Protocol server that lends its
// tools to an agent. The client lives in the separate mcp module, so its
// go-sdk dependency stays out of the core (decisions §5.7); this interface is
// what lets an Agent hold servers without importing it.
type MCPServer interface {
	// Name identifies the server for tracing and tool namespacing.
	Name() string
	// ListTools returns the tools the server currently exposes.
	ListTools(ctx context.Context, rc *RunContext, agent *Agent) ([]*Tool, error)
	// Close releases the server connection.
	Close() error
}
