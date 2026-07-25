// Command otel exports a run's trace as OpenTelemetry spans.
//
// The OTel SDK lives in its own module so it stays out of the core, so this
// example has its own go.mod too:
//
//	cd examples/otel && OPENAI_API_KEY=... go run .
//
// It prints the spans to stderr through OTel's stdout exporter. Swap that for
// an OTLP exporter to ship them at a collector instead — nothing else changes.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/openai"
	"github.com/zzir/agents-go/tracing"
	agentsotel "github.com/zzir/agents-go/tracing/otel"
)

func main() {
	stdout, err := stdouttrace.New(stdouttrace.WithWriter(os.Stderr), stdouttrace.WithPrettyPrint())
	if err != nil {
		log.Fatal(err)
	}

	// NewTracerProvider wires the pinned IDGenerator the exporter needs to
	// rebuild our span tree; build the provider by hand only if it needs
	// options of its own.
	tp, otelExporter, err := agentsotel.NewTracerProvider(sdktrace.WithBatcher(stdout))
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = tp.Shutdown(context.Background()) }()

	// The exporter must be driven by a batch processor: it reconstructs spans
	// by pinning ids, which is stateful and therefore serialized.
	proc := tracing.NewBatchProcessor(otelExporter, tracing.BatchProcessorOptions{})
	defer proc.Shutdown(context.Background())

	weather := agents.NewFunctionTool("get_weather", "Look up the weather for a city.",
		func(_ context.Context, _ *agents.ToolContext, args struct {
			City string `json:"city" jsonschema:"the city to look up"`
		}) (string, error) {
			return "sunny in " + args.City, nil
		})

	agent := &agents.Agent{
		Name:         "assistant",
		Instructions: agents.StaticInstructions("Answer briefly."),
		Model:        "gpt-4o",
		Tools:        []agents.Tool{weather},
	}

	res, err := agents.RunSync(context.Background(), agent, "What is the weather in Paris?", agents.RunOptions{
		Model:   agents.ModelOptions{Provider: openai.NewProvider()},
		Observe: agents.ObserveOptions{Tracer: tracing.NewTracer(proc)},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.FinalOutputString())

	// Flush before exit so the spans reach the exporter.
	proc.ForceFlush()
}
