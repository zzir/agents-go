// Command agents-server runs the agents-go web server: an HTTP + WebSocket API and embedded web UI over the agents SDK.
//
// The comments below are the OpenAPI general info consumed by swag
// (go tool swag init --v3.1, see the Makefile openapi target); regenerate
// internal/docs after changing them or any handler annotation.
//
//	@title						agents-server API
//	@version					1.0
//	@description				REST API of agents-server, the web server over the agents-go SDK: sessions, agents, MCP servers, memories, settings, skills, provider routes, guardrails, and sandboxes. Runs are started over the WebSocket protocol (see the README); this spec covers the HTTP surface.
//	@BasePath					/api/v1
//
//	@securityDefinitions.apikey	BearerAuth
//	@in							header
//	@name						Authorization
//	@description				Value is "Bearer <token>" — the token printed at server startup (or set via --token).
package main

import "github.com/zzir/agents-go/cmd/agents-server/cmd"

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	cmd.SetVersionInfo(version, commit, date)
	cmd.Execute()
}
