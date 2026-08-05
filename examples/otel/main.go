// Command otel exports a run's trace as OpenTelemetry spans.
//
// The OTel SDK lives in its own module so it stays out of the core, so this
// example has its own go.mod too:
//
//	cd examples/otel && OPENAI_API_KEY=... go run .
//
// By default it prints the spans to stderr. Set OTEL_EXPORTER_OTLP_ENDPOINT to
// ship them at a collector instead — nothing but the exporter changes. To see
// them in a UI, run Jaeger, which is a collector and a viewer in one container:
//
//	docker run -d --name jaeger -p 16686:16686 -p 4317:4317 jaegertracing/jaeger:2.11.0
//	cd examples/otel && OPENAI_API_KEY=... OTEL_EXPORTER_OTLP_ENDPOINT=localhost:4317 go run .
//
// then open http://localhost:16686 and pick the "agents-go-example" service.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/openai"
	"github.com/zzir/agents-go/tracing"
	agentsotel "github.com/zzir/agents-go/tracing/otel"
)

// newExporter returns an OTLP exporter when an endpoint is configured and the
// stdout one otherwise, so the example runs with or without a collector.
func newExporter(ctx context.Context) (sdktrace.SpanExporter, string, error) {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		exp, err := stdouttrace.New(stdouttrace.WithWriter(os.Stderr), stdouttrace.WithPrettyPrint())
		return exp, "stdout", err
	}
	// WithInsecure because a local collector has no TLS. Drop it for anything
	// that is not localhost.
	exp, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(),
	)
	return exp, "OTLP → " + endpoint, err
}

func main() {
	ctx := context.Background()

	exporter, where, err := newExporter(ctx)
	if err != nil {
		log.Fatal(err)
	}

	// The service name is what a UI groups traces by; without it everything
	// lands under "unknown_service".
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName("agents-go-example"),
		attribute.String("example", "otel"),
	))
	if err != nil {
		log.Fatal(err)
	}

	// NewTracerProvider wires the pinned IDGenerator the exporter needs to
	// rebuild our span tree; build the provider by hand only if it needs
	// options of its own.
	tp, otelExporter, err := agentsotel.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = tp.Shutdown(ctx) }()

	// The exporter must be driven by a batch processor: it reconstructs spans
	// by pinning ids, which is stateful and therefore serialized.
	proc := tracing.NewBatchProcessor(otelExporter, tracing.BatchProcessorOptions{})
	defer proc.Shutdown(ctx)

	weather := agents.NewTool("get_weather", "Look up the weather for a city.",
		func(_ context.Context, _ *agents.ToolContext, args struct {
			City string `json:"city" jsonschema:"the city to look up"`
		}) (string, error) {
			return "sunny in " + args.City, nil
		})

	agent := &agents.Agent{
		Name:         "assistant",
		Instructions: agents.StaticInstructions("Answer briefly."),
		Model:        "gpt-4o",
		Tools:        []*agents.Tool{weather},
	}

	fmt.Fprintln(os.Stderr, "exporting spans to", where)

	result, err := agents.RunSync(ctx, agent, "What is the weather in Paris?", agents.RunOptions{
		Model:   agents.ModelOptions{Provider: openai.NewProvider()},
		Observe: agents.ObserveOptions{Tracer: tracing.NewTracer(proc)},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(result.FinalOutputString())

	// Flush both stages before exit: ours into the OTel SDK, then the SDK's
	// batcher out over the wire. Skipping either loses the trace.
	proc.ForceFlush()
	if err := tp.ForceFlush(ctx); err != nil {
		log.Print("flushing spans: ", err)
	}
}
