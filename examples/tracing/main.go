// Command tracing demonstrates the tracing pipeline: tracer → batch processor →
// console exporter, one trace per run with agent, model-call and tool spans;
// TraceGroupID links related runs and TraceMetadata stamps every span.
//
// Run with: OPENAI_API_KEY=... go run ./examples/tracing
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/openai"
	"github.com/zzir/agents-go/tracing"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	// Console exporter for demos; swap in your own Exporter to ship spans to a
	// collector.
	exporter := tracing.NewConsoleExporter(os.Stderr)
	processor := tracing.NewBatchProcessor(exporter, tracing.BatchProcessorOptions{})
	defer processor.Shutdown(context.Background())
	tracer := tracing.NewTracer(processor)

	weather := agents.NewTool("get_weather", "Return the weather for a city.",
		func(ctx context.Context, tc *agents.ToolContext, args struct {
			City string `json:"city" jsonschema:"city name"`
		}) (string, error) {
			return "22°C and sunny in " + args.City, nil
		})

	agent := &agents.Agent{
		Name:         "trace-demo",
		Model:        "gpt-4o-mini",
		Instructions: agents.StaticInstructions("Use the weather tool, then answer in one sentence."),
		Tools:        []*agents.Tool{weather},
	}

	res, err := agents.RunSync(context.Background(), agent, "What's the weather in Kyoto?", agents.RunOptions{Model: agents.ModelOptions{Provider: openai.NewProvider()}, Observe: // reads OPENAI_API_KEY
	agents.ObserveOptions{Tracer: tracer, TraceGroupID: "thread-42", TraceMetadata:                                                                                                // one chat thread across runs
	map[string]any{"tenant": "examples"}},                                                                                                                                         // free-form context
	})
	if err != nil {
		return err
	}
	fmt.Println(res.FinalOutputString())
	// The deferred Shutdown flushes the trace and its spans to stderr.
	return nil
}
