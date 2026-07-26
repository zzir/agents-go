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
| `Level` | Override the minimum level for SDK records only |

Most of what the SDK has to say is `Debug`. `Level` exists so a caller can have
that without turning `Debug` on for their whole application:

```go
debug := slog.LevelDebug
opts.Log = agents.LogConfig{Logger: myLogger, Level: &debug}
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

Use the same marker in your own tools:

```go
log.LogAttrs(ctx, slog.LevelDebug, "querying",
	slog.String("table", name),
	agents.Sensitive("filter", userFilter))
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
