// Command logging turns on the SDK's own structured logging and shows the two
// separate switches: whether the SDK logs at all, and whether those records may
// carry conversation content.
//
// Run with: OPENAI_API_KEY=... go run ./examples/logging
package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/openai"
)

func main() {
	provider := openai.NewProvider()

	// Most of what the SDK says is Debug, so give it a logger whose handler
	// enables Debug rather than turning Debug on application-wide.
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	agent := &agents.Agent{
		Name:         "assistant",
		Instructions: agents.StaticInstructions("Answer in one short sentence."),
		Model:        "gpt-4o-mini",
		Tools: []*agents.Tool{
			agents.NewTool("clock", "Return the current UTC time.",
				func(ctx context.Context, tc *agents.ToolContext, _ struct{}) (string, error) {
					return "2026-01-01T00:00:00Z", nil
				}),
		},
	}

	// SensitiveData is off by default and is a separate decision from the
	// logger itself: "log what the SDK is doing" and "log what the user said"
	// are not the same switch. Leave it off and prompts, tool arguments, tool
	// results and model output are withheld from the records.
	opts := agents.RunOptions{
		Model: agents.ModelOptions{Provider: provider},
		Log:   agents.LogConfig{Logger: logger},
	}

	fmt.Println("--- logging on, sensitive data withheld ---")
	res, err := agents.RunSync(context.Background(), agent, "What time is it?", opts)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("answer:", res.FinalOutputString())

	fmt.Println("\n--- the same run with SensitiveData: true ---")
	opts.Log.SensitiveData = true
	if _, err := agents.RunSync(context.Background(), agent, "What time is it?", opts); err != nil {
		log.Fatal(err)
	}

	// A run that survives trouble records it as a Diagnostic rather than
	// failing — worth checking even when err is nil.
	for _, d := range res.Diagnostics {
		fmt.Printf("diagnostic: %s %s\n", d.Code, d.Message)
	}
}
