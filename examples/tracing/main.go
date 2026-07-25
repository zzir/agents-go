// Command tracing demonstrates the tracing pipeline: a tracer wired to a
// batch processor and console exporter records one trace per run, with spans
// for the agent turn, each model call and each tool call. TraceGroupID links
// the traces of related runs (e.g. one chat thread) and TraceMetadata attaches
// arbitrary context — the counterparts of Python's RunConfig group_id /
// trace_metadata.
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
	// Console exporter for demos; swap in NewHTTPExporter (or your own
	// Exporter) to ship spans to a collector instead.
	exporter := tracing.NewConsoleExporter(os.Stderr)
	processor := tracing.NewBatchProcessor(exporter, tracing.BatchProcessorOptions{})
	defer processor.Shutdown(context.Background())
	tracer := tracing.NewTracer(processor)

	weather := agents.NewFunctionTool("get_weather", "Return the weather for a city.",
		func(ctx context.Context, tc *agents.ToolContext, args struct {
			City string `json:"city" jsonschema:"city name"`
		}) (string, error) {
			return "22°C and sunny in " + args.City, nil
		})

	agent := &agents.Agent{
		Name:         "trace-demo",
		Model:        "gpt-4o-mini",
		Instructions: agents.StaticInstructions("Use the weather tool, then answer in one sentence."),
		Tools:        []agents.Tool{weather},
	}

	res, err := agents.RunSync(context.Background(), agent, "What's the weather in Kyoto?", agents.RunOptions{
		ModelProvider: openai.NewProvider(), // reads OPENAI_API_KEY
		Tracer:        tracer,
		TraceGroupID:  "thread-42",                          // one chat thread across runs
		TraceMetadata: map[string]any{"tenant": "examples"}, // free-form context
	})
	if err != nil {
		return err
	}
	fmt.Println(res.FinalOutputString())
	// The deferred Shutdown flushes the trace and its agent / generation /
	// function spans to stderr.
	return nil
}
