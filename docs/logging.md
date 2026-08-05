# Logging

The SDK emits structured [`log/slog`](https://pkg.go.dev/log/slog) records
describing what a run is doing. It is **off by default**: an SDK that logs to
the process default the moment it is imported is one that shows up uninvited in
somebody's production output.

```go
opts.Log = agents.LogConfig{Logger: slog.Default()}
```

| Field | Meaning |
|---|---|
| `Logger` | Where records go. Nil disables SDK logging entirely |
| `SensitiveData` | Include conversation content — prompts, tool arguments. Off by default |

The logger's own handler sets the level floor. Most of what the SDK has to say
is `Debug`, so hand it a dedicated logger whose handler enables `Debug` to see
it without turning `Debug` on for your whole application:

```go
h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})
opts.Log = agents.LogConfig{Logger: slog.New(h)}
```

## Sensitive data is a separate decision

"Log what the SDK is doing" and "log what the user said" are different choices,
and the second one puts a conversation into a log aggregator. It has to be made
on purpose.

Attributes carrying content are marked with `agents.Sensitive` and dropped
unless `SensitiveData` is set — the record itself still appears, without them:

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

Logging and [tracing](tracing.md) answer different questions and are configured
separately. A trace reconstructs one run's structure — spans, timings, parentage
— for a debugger looking at that run. Logs are a stream for an operator watching
many runs. `Observe.IncludeSensitiveData` and `Log.SensitiveData` are likewise
separate: exporting spans to a tracing backend and writing lines to a log file
are different exposures.

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

The point is the failures that **do not fail the run**: three retries, a
fallback to a slower model, a compaction pass that gave up, a recovered tool
panic. None of them reach an error return, so a run that answered after a bad
time looks identical to one that answered first time.

| Type | Recorded when |
|---|---|
| `model_retry` | A model call failed and was retried |
| `model_fallback` | A backup model or provider answered |
| `stream_error` | A stream broke after emitting, so it could not be retried |
| `tool_panic` | A tool panicked and was recovered |
| `tool_timeout` | A tool hit its deadline |
| `compaction_failed` | A compaction pass failed; the run continued uncompacted |
| `response_truncated` | A response was cut off and its tool calls refused |

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
