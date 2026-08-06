package agents

import "context"

// MCPServer is a connection to a Model Context Protocol server that lends its
// tools to an agent. Tools are all the runner needs; the rest of the protocol
// (prompts, resources) is reached through the concrete client instead. The full
// client lives in the github.com/zzir/agents-go/mcp module, which is separate
// precisely so its go-sdk dependency stays out of the core; this interface is
// what lets the Agent struct hold a list of servers without importing it.
type MCPServer interface {
	// Name identifies the server for tracing and tool namespacing.
	Name() string
	// ListTools returns the tools the server currently exposes.
	ListTools(ctx context.Context, rc *RunContext, agent *Agent) ([]*Tool, error)
	// Close releases the server connection.
	Close() error
}
