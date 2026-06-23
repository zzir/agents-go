// Command agents-server runs the agents-go web server: an HTTP + WebSocket API and embedded web UI over the agents SDK.
package main

import "github.com/zzir/agents-go/cmd/agents-server/cmd"

func main() {
	cmd.Execute()
}
