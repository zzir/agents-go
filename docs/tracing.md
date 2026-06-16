# Tracing

The SDK can record a trace of every run: one trace per `Run`, with spans for each agent, model call, tool call, handoff and guardrail. Tracing is **opt-in**: build a tracer, pass it in `RunOptions.Tracer`, and pick where the data goes.

```go
import "github.com/zzir/agents-go/tracing"

exporter := tracing.NewConsoleExporter(os.Stderr)
processor := tracing.NewBatchProcessor(exporter, tracing.BatchProcessorOptions{})
defer processor.Shutdown(context.Background())

tracer := tracing.NewTracer(processor)

res, err := agents.Run(ctx, agent, input, agents.RunOptions{
	ModelProvider: provider,
	Tracer:        tracer,
})
```

## What gets recorded

| Span | `Type` | Covers |
|---|---|---|
| `agent:<name>` | `SpanTypeAgent` | One agent's tenure (per handoff segment); parent of the spans below |
| `generation:<name>` | `SpanTypeGeneration` | One model call (records `response_id` and per-call `input_tokens`/`output_tokens`/`total_tokens`) |
| `function:<tool>` | `SpanTypeFunction` | One function tool invocation (errors recorded) |
| `handoff:<tool>` | `SpanTypeHandoff` | A handoff execution |
| `guardrail:input` / `guardrail:output` | `SpanTypeGuardrail` | Guardrail batches (tripwires recorded as errors) |

Each span carries a `Type` field (one of the `tracing.SpanType*` constants) so a processor can dispatch on `span.Type` instead of parsing `span.Name`, plus structured `Data` keys (`"name"`, `"stage"`, `"response_id"`). The runner creates these via the typed constructors (`StartAgentSpan`, `StartGenerationSpan`, `StartFunctionSpan`, `StartHandoffSpan`, `StartGuardrailSpan`); the untyped `StartSpan` remains for custom spans and leaves `Type` empty. This is the idiomatic-Go stand-in for Python's typed `SpanData` subclasses — a `Type` tag plus a `Data` map rather than a sealed union.

Streamed runs, resumed (HITL) runs and nested agent-as-tool runs are traced too; nested runs join the parent's trace rather than starting their own.

IDs follow the Python SDK's format (`trace_<32 hex>`, `span_<24 hex>`) and are generated from `crypto/rand`.

## Pipeline

```
Tracer  ──►  Processor (when spans start/end)  ──►  Exporter (where batches go)
```

- **`tracing.NewTracer(proc)`** — hands trace/span lifecycle events to a `Processor`. A nil tracer (or processor) is a no-op, so library code never needs nil checks.
- **`tracing.NewBatchProcessor(exporter, opts)`** — buffers items and flushes them on a background goroutine. Options: `MaxBatchSize` (128), `FlushInterval` (5s), `MaxQueueSize` (8192, overflow dropped and counted — see `Dropped()`). **Call `Shutdown(ctx)` before exit** to flush; traces are exported at start (so a crash cannot orphan all spans) and spans at finish.
- **Exporters**: `ConsoleExporter` (human-readable lines), `HTTPExporter` (JSON batches via POST), `CollectingExporter` (tests), `NoopExporter`, or any `func(items []any)` as `FuncExporter`.

```go
exporter := tracing.NewHTTPExporter("https://telemetry.example.com/batches", tracing.HTTPExporterOptions{
	Headers: map[string]string{"Authorization": "Bearer …"},
})
```

`HTTPExporter` posts `{"data": [...]}` batches of `Trace`/`Span` objects, drops failed batches without retry (counted via `Dropped()`), and is **not** the OpenAI traces-dashboard wire format — point it at your own collector.

## Custom processors

Implement `tracing.Processor` to integrate with an existing telemetry stack (e.g. bridge to OpenTelemetry) — the runner only ever talks to the interface:

```go
type Processor interface {
	OnTraceStart(t *Trace)
	OnTraceEnd(t *Trace)
	OnSpanStart(s *Span)
	OnSpanEnd(s *Span)
	ForceFlush()
	Shutdown(ctx context.Context)
}
```

Span callbacks can fire from concurrent goroutines (parallel tools, input guardrails) — processors must be goroutine-safe.

## Sensitive data

Spans record names, timing, error messages and small attributes such as `response_id` — not prompts, completions or tool payloads. If you add attributes from your own hooks, apply your data policies accordingly.
