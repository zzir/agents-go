package agents

import "context"

// MCPServer is a connection to a Model Context Protocol server that exposes
// tools (and prompts) to an agent. The full client lives in the mcp subpackage;
// this interface is declared here so the Agent struct can hold a list of servers
// without importing that package.
type MCPServer interface {
	// Name identifies the server for tracing and tool namespacing.
	Name() string
	// ListTools returns the tools the server currently exposes.
	ListTools(ctx context.Context, rc *RunContext, agent *Agent) ([]*Tool, error)
	// Close releases the server connection.
	Close() error
}
