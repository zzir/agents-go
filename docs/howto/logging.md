# Logging

The SDK emits structured [`log/slog`](https://pkg.go.dev/log/slog) records
describing what a run is doing. It is **off by default** and never writes to
`slog.Default()` on its own ([spec §2.11c](../reference/spec.md#211c-logging)).

```go
opts.Log = agents.LogConfig{Logger: slog.Default()}
```

`LogConfig` has two fields — `Logger` (nil disables SDK logging entirely) and
`SensitiveData` ([pkg.go.dev](https://pkg.go.dev/github.com/zzir/agents-go/agents#LogConfig)).

The logger's own handler sets the level floor. Most of what the SDK has to say
is `Debug`, so hand it a dedicated logger whose handler enables `Debug` to see
it without turning `Debug` on for your whole application:

```go
h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})
opts.Log = agents.LogConfig{Logger: slog.New(h)}
```

## Sensitive data is a separate decision

Attributes carrying conversation content are marked with `agents.Sensitive`
and dropped unless `SensitiveData` is set — a second opt-in, because logging
what the SDK does and logging what the user said are different exposures
([spec §2.11c](../reference/spec.md#211c-logging)). The record itself still
appears, without them:

```
level=DEBUG msg="calling model" component=run agent=support turn=2 input_items=7 tools=3
```

With `SensitiveData: true` the same record also carries `instructions=…` and
tool `arguments=…`.

Use the same marker in your own tools. Outside the SDK's opt-in filter — on
your own `slog.Logger` — a `Sensitive` attribute renders as `«redacted»`, never
the value, so marking is safe by default:

```go
log.LogAttrs(ctx, slog.LevelDebug, "querying",
	slog.String("table", name),
	agents.Sensitive("filter", userFilter)) // renders filter=«redacted» here
```

## What gets logged

Every record carries `component`, so the SDK's chatter can be filtered by where
it came from.

| Component | Records |
|---|---|
| `run` | run started, turn started, calling model, model responded, handoff, turn budget exhausted, turn persisted |
| `tool` | tool started / finished / failed, waiting for approval, truncated response refused |
| `compaction` | context compacted, compaction pass failed |

Levels: `Info` for things that change the run's course (start, handoff, budget
exhausted, compaction), `Debug` for the per-turn detail, `Warn` and `Error` for
what went wrong.

## Relationship to tracing

Logging and [tracing](tracing.md) are configured separately, including their sensitive-data switches — `Log.SensitiveData` here, `Observe.IncludeSensitiveData` for [spans](tracing.md#sensitive-data).

## Diagnostics: trouble a run survived

Logs answer "what happened, across everything". Diagnostics answer "what went
wrong *in this run*", and they are attached to the run and its session rather
than written to a stream:

```go
res, _ := agents.RunSync(ctx, agent, input, opts)
for _, d := range res.Diagnostics {
	fmt.Printf("%s: %s %v\n", d.Type, d.Message, d.Details)
}
// model_retry: upstream unavailable map[attempt:1 max_attempts:4 streaming:false]
// model_fallback: … map[used_index:1 models:2 streaming:false]
```

They record the failures that do **not** fail the run — retries, a fallback, a compaction pass that gave up, a recovered tool panic — which no error return would ever show ([spec §2.11d](../reference/spec.md#211d-diagnostics)).

| Type | Recorded when |
|---|---|
| `model_retry` | A model call failed and was retried |
| `model_fallback` | A backup model or provider answered |
| `stream_error` | A stream broke after emitting, so it could not be retried. Retry and fallback each record one for the same break — `attempt` names the attempt, `used_index` the backend |
| `tool_panic` | A tool panicked and was recovered |
| `tool_timeout` | A tool hit its deadline |
| `compaction_failed` | A compaction pass failed; the run continued uncompacted. `details.point` names the moment — a `CompactionPoint`, or `overflow_recovery` for a session write that abandoned an overflow recovery |
| `response_truncated` | A response was cut off and its tool calls refused |
| `context_overflow` | A model call did not fit the context window; the run compacted and retried ([overflow recovery](sessions.md)) |

With a [Session](sessions.md), each diagnostic is stored on the entry for the
turn it happened in, so the session explains itself long after any log has
rotated. A failed run reports them too, on `RunError.Result.Diagnostics` — the
error is the last straw, the diagnostics are what led to it.

Report your own from a tool or a custom `Model` decorator:

```go
agents.RecordDiagnostic(ctx, "cache_miss", err, map[string]any{"key": k})
```

It is a no-op when there is no run behind the context, so a decorator used
standalone still works.

A runnable program is [examples/logging](../../examples/logging/main.go), which runs the same agent twice — once with `SensitiveData` off, once with it on — so the difference in the records is visible side by side.
