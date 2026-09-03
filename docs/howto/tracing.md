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
| `function:<tool>` | `SpanTypeFunction` | One function tool invocation (records `call_id`; errors recorded) |
| `handoff:<tool>` | `SpanTypeHandoff` | A handoff execution |
| `guardrail:input` / `guardrail:output` | `SpanTypeGuardrail` | Guardrail batches (tripwires recorded as errors) |
| `compaction` | `SpanTypeCompaction` | A compaction pass (entries before/after) |
| `model_retry` | `SpanTypeModelRetry` | One retried model call, nested under the generation span it belongs to |
| `mcp.list_tools` / `mcp.call_tool` | `SpanTypeMCP` | An MCP server round trip |
| `sandbox.exec` / `sandbox.apply_patch` | `SpanTypeSandbox` | A sandbox operation (exit code, timeout) |

A retry span is a zero-duration marker rather than a wrapper ([spec §2.11e](../reference/spec.md#211e-span-coverage)). The `compaction` span is opened lazily via `CompactionArgs.StartSpan`, only when a `session.CompactionAware` session actually compacts — annotated with `"before_items"`/`"after_items"` and any compaction error; a no-op pass emits no span.

Each span carries a `Type` field (one of the `tracing.SpanType*` constants) so a processor can dispatch on `span.Type` instead of parsing `span.Name`, plus structured `Data` keys (`"name"`, `"stage"`, `"response_id"`). The runner sets `Type` on the spans it opens; a span you open yourself through `StartSpanFrom` (below) names its own type, and `TraceHandle.StartSpan` / `SpanHandle.StartSpan` leave it empty.

Streamed runs, resumed (HITL) runs and nested agent-as-tool runs are traced too; nested runs join the parent's trace rather than starting their own, and their agent spans are parented under the `function:` span of the tool call that triggered them, so the tree shows which call owns each nested run.

`RunOptions.Observe.TraceGroupID` and `RunOptions.Observe.TraceMetadata` stamp the trace at start — use them to link the traces of one chat thread or attach tenant info. Set them via options rather than mutating the `Trace` afterwards, which would race with background exporting.

IDs are `trace_<32 hex>` / `span_<16 hex>`, the shape trace backends already parse, generated from `crypto/rand`.

## Sensitive data

By default each generation span records the full request body: `"model"`,
`"system_instructions"`, `"input"` (the exact items sent, after session
history, compaction, and input filters were applied), `"tools"` (name,
description, and parameter schema per tool), `"model_settings"` (the resolved
settings; the `Extra*` passthrough fields are excluded), `"handoffs"` (tool
name, agent name, description, and input schema per handoff), `"output_schema"`
(`name`, `schema`, `strict`), `"prompt"`, `"previous_response_id"`/`"conversation_id"`,
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
		IncludeSensitiveData: &include, // nil means include (the default)
	},
})
```

The caller decides; the SDK reads no environment variable for this ([spec §2.14](../reference/spec.md#214-the-sdk-reads-no-environment-variable)). Opting out keeps names, timing, error messages and the small ids — `response_id` and `call_id` stay on the span either way, so a consumer can still join a span to the session entry it produced — and drops prompts, completions and tool payloads. Attributes you add from your own hooks are yours to police. The logging switch, `Log.SensitiveData`, is a separate decision ([Logging](logging.md)).

## Pipeline

```
Tracer  ──►  Processor (when spans start/end)  ──►  Exporter (where batches go)
```

- **`tracing.NewTracer(proc)`** — hands trace/span lifecycle events to a `Processor`. A nil tracer (or processor) is a no-op, so library code never needs nil checks.
- **`tracing.NewBatchProcessor(exporter, opts)`** — buffers items and flushes them on a background goroutine. Options: `MaxBatchSize` (128), `FlushInterval` (5s), `MaxQueueSize` (8192, overflow dropped and counted — see `Dropped()`). **Call `Shutdown(ctx)` before exit** to flush; traces are exported at start (so a crash cannot orphan all spans) and spans at finish.
- **Exporters**: `ConsoleExporter` (human-readable lines), `CollectingExporter`
  (tests), or any `func(items []tracing.Item)` as `FuncExporter`.

```go
// Anything that takes a batch is an exporter.
exporter := tracing.FuncExporter(func(items []tracing.Item) {
	for _, it := range items {
		_ = it // ship it wherever you like
	}
})
```

There is no built-in HTTP exporter: the span record is OTel-shaped — 8-byte span ids, 16-byte trace ids, one root span per agent (so an exporter grouping by trace carries workflow metadata across roots itself) — and a `Processor` or `Exporter` feeding your collector is the few lines you write against it ([decisions §5.6b](../explanation/decisions.md#56b-tracing-stays-vendor-neutral-otel-export-is-the-consumers-job)).

## Custom processors

Implement [`tracing.Processor`](https://pkg.go.dev/github.com/zzir/agents-go/tracing#Processor) to integrate with an existing telemetry stack — the runner only ever talks to the interface. Span callbacks can fire from concurrent goroutines (parallel tools, input guardrails), so a processor must be goroutine-safe.

## Instrumenting your own code

A subsystem the runner does not own — a custom `Model` decorator, a tool that
calls a service — nests its work under the run by reading the current span off
the context:

```go
func (m *myModel) Respond(ctx context.Context, req agents.ModelRequest) (*agents.ModelResponse, error) {
	span, ctx := tracing.StartSpanFrom(ctx, "cache.lookup", "cache", nil)
	defer span.Finish()
	…
}
```

The context is the channel and `StartSpanFrom` returns a usable no-op handle when there is no trace; the runner installs the generation span as the parent during a model call and the function span during a tool invocation, so retries, MCP round trips and sandbox execs nest under the call that caused them ([spec §2.11e](../reference/spec.md#211e-span-coverage)).
