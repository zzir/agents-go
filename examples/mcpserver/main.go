// Command mcpserver demonstrates serving SDK tools over MCP: the same tool
// values an Agent runs, handed to an editor, a desktop client or another agent.
//
// A real server runs over stdio and is launched BY the client:
//
//	srv, _ := mcp.NewToolServer(tools, mcp.ServeOptions{Name: "my-tools"})
//	log.Fatal(mcp.ServeStdio(context.Background(), srv))
//
// This program connects a client to it in-process instead, so it terminates and
// prints what a client would see.
//
// Run with: go run ./examples/mcpserver   (no API key needed)
package main

import (
	"context"
	"fmt"
	"log"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/mcp"
)

type unitArgs struct {
	Celsius float64 `json:"celsius" jsonschema_description:"Temperature in Celsius."`
}

// main keeps the defers honest: log.Fatal does not run them, so the work lives
// in run() and only the reporting happens up here.
func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx := context.Background()

	toC := agents.NewFunctionTool("to_fahrenheit", "Convert Celsius to Fahrenheit.",
		func(_ context.Context, _ *agents.ToolContext, a unitArgs) (string, error) {
			return fmt.Sprintf("%.1f°F", a.Celsius*9/5+32), nil
		})

	srv, err := mcp.NewToolServer([]agents.Tool{toC}, mcp.ServeOptions{
		Name:         "unit-tools",
		Instructions: "Unit conversions.",
	})
	if err != nil {
		return err
	}

	// In-process transport: what stdio does, minus the pipes.
	clientT, serverT := mcpsdk.NewInMemoryTransports()
	go func() {
		if err := srv.Run(ctx, serverT); err != nil {
			log.Println("server:", err)
		}
	}()

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "demo", Version: "0.1.0"}, nil)
	sess, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		return err
	}
	defer sess.Close()

	list, err := sess.ListTools(ctx, nil)
	if err != nil {
		return err
	}
	fmt.Println("tools this server offers:")
	for _, t := range list.Tools {
		// The schema travels with the tool, so a client can validate a call
		// before making it.
		fmt.Printf("  %s — %s (schema: %v)\n", t.Name, t.Description, t.InputSchema != nil)
	}

	res, err := sess.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "to_fahrenheit",
		Arguments: map[string]any{"celsius": 21.0},
	})
	if err != nil {
		return err
	}
	for _, c := range res.Content {
		if tc, ok := c.(*mcpsdk.TextContent); ok {
			fmt.Println("\n21°C is", tc.Text)
		}
	}
	return nil
}
