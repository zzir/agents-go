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

A retry span is a zero-duration marker rather than a wrapper: by the time an
attempt is known to have failed it has already happened, and the point is that
it happened at all. A generation span that took eight seconds because it was
tried three times is otherwise indistinguishable from one that was simply slow.

Each span carries a `Type` field (one of the `tracing.SpanType*` constants) so a processor can dispatch on `span.Type` instead of parsing `span.Name`, plus structured `Data` keys (`"name"`, `"stage"`, `"response_id"`). The runner creates these via the typed constructors (`StartAgentSpan`, `StartGenerationSpan`, `StartFunctionSpan`, `StartHandoffSpan`, `StartGuardrailSpan`); the untyped `StartSpan` remains for custom spans and leaves `Type` empty.

Streamed runs, resumed (HITL) runs and nested agent-as-tool runs are traced too; nested runs join the parent's trace rather than starting their own, and their agent spans are parented under the `function:` span of the tool call that triggered them, so the tree shows which call owns each nested run.

`RunOptions.Observe.TraceGroupID` and `RunOptions.Observe.TraceMetadata` stamp the trace at start — use them to link the traces of one chat thread or attach tenant info. Set them via options rather than mutating the `Trace` afterwards, which would race with background exporting.

### Sensitive data on generation spans

When a run's session compacts its history (a `session.CompactionAware`), the
runner wraps the pass in a span of type `"compaction"` — opened lazily via
`CompactionArgs.StartSpan` only when the session actually compacts, annotated
by the session with `"before_items"`/`"after_items"`, and carrying any
compaction error. No-op passes emit no span.

By default each generation span also records the full request body: `"model"`,
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

When the option is nil the span includes content — the default. The SDK reads
no environment variable for this; the caller decides (spec §2.14). Opting out
keeps ids and token usage, drops content.

IDs are `trace_<32 hex>` / `span_<16 hex>`, the shape trace backends already parse, generated from `crypto/rand`.

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

There is no built-in HTTP exporter. Sending spans over the wire means picking a
format, and every collector wants a different one — so the SDK exports to a
function and lets you write the six lines that match yours. That includes
OpenTelemetry: the span record is OTel-shaped (8-byte span ids, 16-byte trace
ids), and a `Processor` or `Exporter` that feeds an OTel SDK is yours to write
against your collector ([decisions §5.6b](../explanation/decisions.md)).

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

Spans record names, timing, error messages and small attributes such as `response_id` and `call_id` — not prompts, completions or tool payloads. Those two ids stay on the span with sensitive data off, so a consumer can still join a span to the session entry it produced. If you add attributes from your own hooks, apply your data policies accordingly.

Two shapes an exporter can rely on ([decisions §5.6b](../explanation/decisions.md)):

- **Span ids are 8 bytes and trace ids 16** (`tracing.NewSpanID`,
  `tracing.NewTraceID`) — the OTel widths ([decisions §5.6b](../explanation/decisions.md#56b-tracing-stays-vendor-neutral-otel-export-is-the-consumers-job)), so an OTel-shaped consumer never
  truncates.
- **A trace has a root span per agent**, not one per trace: a handoff ends the
  current agent span and starts the next one at the top level, so an exporter
  grouping by trace must carry workflow metadata across roots itself.

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

The context is the channel because those receive one and nothing else belonging
to the run; threading a span handle through every signature would mean each
implementation had to forward it, and the one that forgot would silently orphan
its spans at the root.

`StartSpanFrom` returns a usable no-op handle when there is no trace, so an
instrumented call site never needs a branch — and the subsystem behaves exactly
as it did before it was instrumented when used outside a run.

The runner installs the right parent at each point: the **generation** span for
the model call (so retries nest under it) and the **function** span for a tool
invocation (so an MCP round trip or a sandbox exec shows up under the call that
caused it).
