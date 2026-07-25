# Tracing

The SDK can record a trace of every run: one trace per `Run`, with spans for each agent, model call, tool call, handoff and guardrail. Tracing is **opt-in**: build a tracer, pass it in `RunOptions.Observe.Tracer`, and pick where the data goes.

```go
import "github.com/zzir/agents-go/tracing"

exporter := tracing.NewConsoleExporter(os.Stderr)
processor := tracing.NewBatchProcessor(exporter, tracing.BatchProcessorOptions{})
defer processor.Shutdown(context.Background())

tracer := tracing.NewTracer(processor)

res, err := agents.RunSync(ctx, agent, input, agents.RunOptions{
	Model: agents.ModelOptions{Provider: provider},
	Observe: agents.ObserveOptions{Tracer: tracer},
})
```

## What gets recorded

| Span | `Type` | Covers |
|---|---|---|
| `agent:<name>` | `SpanTypeAgent` | One agent's tenure (per handoff segment); parent of the spans below |
| `generation:<name>` | `SpanTypeGeneration` | One model call (records `response_id`, per-call `input_tokens`/`output_tokens`/`total_tokens`, and — see below — the full request/response) |
| `function:<tool>` | `SpanTypeFunction` | One function tool invocation (errors recorded) |
| `handoff:<tool>` | `SpanTypeHandoff` | A handoff execution |
| `guardrail:input` / `guardrail:output` | `SpanTypeGuardrail` | Guardrail batches (tripwires recorded as errors) |

Each span carries a `Type` field (one of the `tracing.SpanType*` constants) so a processor can dispatch on `span.Type` instead of parsing `span.Name`, plus structured `Data` keys (`"name"`, `"stage"`, `"response_id"`). The runner creates these via the typed constructors (`StartAgentSpan`, `StartGenerationSpan`, `StartFunctionSpan`, `StartHandoffSpan`, `StartGuardrailSpan`); the untyped `StartSpan` remains for custom spans and leaves `Type` empty. This is the idiomatic-Go stand-in for Python's typed `SpanData` subclasses — a `Type` tag plus a `Data` map rather than a sealed union.

Streamed runs, resumed (HITL) runs and nested agent-as-tool runs are traced too; nested runs join the parent's trace rather than starting their own, and their agent spans are parented under the `function:` span of the tool call that triggered them, so the tree shows which call owns each nested run.

`RunOptions.Observe.TraceGroupID` and `RunOptions.Observe.TraceMetadata` (the counterparts of Python's `RunConfig.group_id` / `trace_metadata`) stamp the trace at start — use them to link the traces of one chat thread or attach tenant info. Set them via options rather than mutating the `Trace` afterwards, which would race with background exporting.

### Sensitive data on generation spans

When a run's session compacts its history (a `CompactionAwareSession`), the
runner wraps the pass in a span of type `"compaction"` — opened lazily via
`CompactionArgs.StartSpan` only when the session actually compacts, annotated
by the session with `"before_items"`/`"after_items"`, and carrying any
compaction error. No-op passes emit no span.

By default each generation span also records the full request body: `"model"`,
`"system_instructions"`, `"input"` (the exact items sent, after session
history, compaction, and input filters were applied), `"tools"` (name,
description, and parameter schema per tool), `"model_settings"` (the resolved
settings; the `Extra*` passthrough fields are excluded), `"handoffs"`,
`"output_schema"`, `"prompt"`, `"previous_response_id"`/`"conversation_id"`,
and `"output"` (the items returned). Streamed calls additionally record
`"time_to_first_token_ms"`. Function spans record the tool call's `"input"`
(arguments JSON) and stringified `"output"`. This is what makes a trace answer
"what did the model actually see?". Because spans flow through exporters, this
content leaves the process — disable it when exporting somewhere conversation
content must not go:

```go
include := false
agents.Run(ctx, agent, input, agents.RunOptions{
	Observe: agents.ObserveOptions{
		Tracer:               tracer,
		IncludeSensitiveData: &include, // nil reads the env var below
	},
})
```

When the option is nil, the `OPENAI_AGENTS_TRACE_INCLUDE_SENSITIVE_DATA`
environment variable decides: anything but `false` means include. This mirrors
the Python SDK's `RunConfig.trace_include_sensitive_data`. Opting out keeps
ids and token usage, drops content.

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

## OpenTelemetry

`tracing/otel` exports our traces as OTel spans. It is a **separate module** —
the OTel SDK is a heavy dependency with its own release cadence, and the core
stays vendor-neutral ([spec.md §5.7](spec.md)).

```go
import agentsotel "github.com/zzir/agents-go/tracing/otel"

tp, exp, err := agentsotel.NewTracerProvider(sdktrace.WithBatcher(otlpExporter))
if err != nil { return err }
defer tp.Shutdown(ctx)

proc := tracing.NewBatchProcessor(exp, tracing.BatchProcessorOptions{})
tracer := tracing.NewTracer(proc)
```

Runnable version: [`examples/otel`](../examples/otel/main.go).

### How the tree survives

Our spans are flat records with string ids, exported in batches *after* they
finish — usually children before parents. OTel builds its tree from live spans
nested through a context, which no longer exists by then.

The exporter rebuilds it by **pinning**: it sets a custom `IDGenerator` to the
exact ids the span already has, injects the parent as a remote `SpanContext`,
and starts and ends the span with its original timestamps. Trace ids, span ids,
parent links and durations all survive, including when a child is exported
before its already-finished parent.

Two consequences:

- **Drive it with a batch processor.** The pinning is stateful, so `Export`
  serializes; it is not usable as a synchronous per-span processor.
- **Span ids are 8 bytes** (`tracing.NewSpanID`) because that is an OTel span
  id. Widening them would force this exporter — and any other OTel-shaped one —
  to truncate silently.

### Attribute mapping

Pinned to GenAI semantic conventions **v1.38.0** (they are still experimental
upstream and have renamed keys between releases, so the version is recorded in
`attributes.go` rather than tracking whatever the SDK ships).

| Our span | OTel name | Key attributes |
|---|---|---|
| agent | `invoke_agent {name}` | `gen_ai.operation.name`, `gen_ai.agent.name` |
| generation | `chat {model}` | `gen_ai.provider.name`, `gen_ai.request.model`, `gen_ai.response.id`, `gen_ai.usage.*` |
| function | `execute_tool {name}` | `gen_ai.operation.name`, `gen_ai.tool.name` |
| handoff | `handoff` | `agents.handoff.tool` |
| guardrail | `guardrail` | `agents.guardrail.stage` |
| compaction | `compact` | `agents.compaction.before_items` / `after_items` |

Concepts with no GenAI equivalent use an `agents.` prefix rather than a
`gen_ai.` one that would imply a convention covering them. An error maps to
`codes.Error` plus `error.type`, carrying the SDK's `ErrorCode` — a stable,
low-cardinality value, which is what the convention asks for.

The workflow name and trace group id land on the trace's **root span only**;
repeating them on every child would multiply a constant across the trace.
