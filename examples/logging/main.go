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

	// Most SDK records are Debug; enable Debug on this logger, not application-wide.
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

	// SensitiveData is a second switch, off by default: prompts, tool arguments,
	// results and model output stay out of the records — see docs/howto/logging.md.
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

	// Trouble a run survived lands in Diagnostics, even when err is nil — see
	// spec §2.11d.
	for _, d := range res.Diagnostics {
		fmt.Printf("diagnostic: %s %s\n", d.Code, d.Message)
	}
}
