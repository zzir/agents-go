# Streaming

`agents.Run` returns a run as a **stream** plus a control handle. Nothing
executes until the stream is ranged.

```go
stream, ctrl := agents.Run(ctx, agent, "Tell me 5 jokes.", agents.RunOptions{Model: agents.ModelOptions{Provider: provider}})

var res *agents.RunResult
for event, err := range stream {
	if err != nil {
		log.Fatal(err) // terminal error; iteration stops
	}
	switch ev := event.(type) {
	case *agents.RawResponsesStreamEvent:
		// token-by-token deltas, verbatim from the Responses API
		if ev.Data.Type == "response.output_text.delta" {
			fmt.Print(ev.Data.Delta) // union exposes the variant fields flattened
		}
	case *agents.RunItemStreamEvent:
		fmt.Printf("\n[%s]\n", ev.Name) // e.g. message_output_created, tool_called, tool_output
	case *agents.AgentUpdatedStreamEvent:
		fmt.Printf("\n[now talking to %s]\n", ev.NewAgent.Name)
	case *agents.RunCompletedEvent:
		res = ev.Result // the finished run, delivered as the last event
	}
}
_ = ctrl
```

`RunStream` is an `iter.Seq2[StreamEvent, error]` — Go's range-over-func
is the whole streaming API.

For the result and nothing else, use `RunSync`:

```go
res, err := agents.RunSync(ctx, agent, "Tell me 5 jokes.", opts)
```

`RunSync` is not merely `Run` with the events discarded — it also calls the
model **without** streaming, since nobody is watching the deltas. That is the
only behavioral difference between the two entry points. If you hold a stream
you have not started ranging, `stream.Collect()` folds it to the result.

**A stream is single-use.** Ranging it *is* the run, so ranging it a second
time — including `Collect()` after you have already broken out of a range loop
— would re-execute everything: another model call, tools re-running their side
effects, duplicate items in the session. That second range yields a
`*UserError` instead. To stop early and keep what you have, collect the events
as you range them; to stop the run, just `break`.

## The run happens on your goroutine

Ranging the stream *is* the run. There is no producer goroutine and no channel
between you and the loop.

That has one consequence worth stating plainly: **abandoning the stream stops
the run.** A `break`, an early `return`, a failing assertion — the loop unwinds
and the run ends where it stood, mid-turn. Nothing leaks, and there is no
context you have to remember to cancel.

It has a second: **a slow consumer slows the run.** For a single consumer that
is backpressure working correctly. For one run feeding several consumers at
different speeds — a server broadcasting to several browsers — put a
[`Fanout`](#fanning-out-to-many-consumers) between them.

And one exception: **a tool streaming progress yields from its own
goroutine.** While a turn's tools run, a `*ToolProgressEvent` reaches your
range body on the goroutine of the tool that emitted it. Every yield is
serialized by a mutex, so the body never runs concurrently with itself — but
it does not always run on the goroutine that started the range. Treat events
as data and this is invisible; code that pins work to the starting goroutine —
a thread-locked UI toolkit, goroutine-local state — should hand the event off
(a channel, a dispatch queue) rather than act in place.

## Event types

### Raw response events

`*RawResponsesStreamEvent` passes through every OpenAI Responses streaming event
(`response.created`, `response.output_text.delta`, …). Use these for token-level
UI streaming. Only `Run` produces them; `RunSync` makes one blocking call.

### Run item events and agent events

`*RunItemStreamEvent` fires when an item is **complete** (a full message, a tool
call, a tool result) — the right granularity for "Fetching the weather…"-style
progress, ignoring per-token noise. Names: `message_output_created`,
`tool_called`, `tool_output`, `handoff_requested`, `handoff_occured`,
`reasoning_item_created`, `injected_input_created`.

A handoff surfaces as **both** `tool_called` and `handoff_requested`: the model
called a tool, and that tool was a handoff.

`*AgentUpdatedStreamEvent` fires once for the starting agent, then on each
handoff.

### Tool progress events

`*ToolProgressEvent` is a partial result pushed by a running tool via
[`ToolContext.Emit`](tools.md#streaming-partial-results). It carries the tool
name, the call id and a partial `ToolResult`:

```go
if p, ok := event.(*agents.ToolProgressEvent); ok {
	render(p.CallID, p.Result)   // key on CallID: several tools stream at once
}
```

Progress **never reaches the model** — the tool's return value does — and it
stops the moment the tool returns, so a card can switch from "running" to the
final result without guessing.

`sandbox.CodeTool` streams stdout this way, and an agent-as-tool forwards its
nested agent's messages.

### The completion event

`*RunCompletedEvent` is terminal and carries the finished `*RunResult`. It is
emitted exactly once, last, on a run that ends without error — which is how a
stream carries its result, and why there is no separate call to forget.

## Controlling a live run

`RunControl` is safe to use from another goroutine, including before you start
ranging.

- `ctrl.StopAfterTurn()` requests a **graceful** stop: the in-flight turn
  finishes — tool calls and session save included — and the run then stops
  cleanly before the next turn, with no error and a nil `FinalOutput`. **This is
  the one that leaves the session consistent**: breaking out of the range loop
  stops mid-turn, and cancelling the context does the same, harder.
- `ctrl.Steer(...)` / `ctrl.NextTurn(...)` / `ctrl.FollowUp(...)` put input into
  a run that is already going — see
  [Steering a run in flight](running_agents.md#steering-a-run-in-flight).

Progress — which agent is up, which turn it is, what the run is doing — is read
from the stream's own events (`AgentUpdatedStreamEvent`, `RunItemStreamEvent`),
not from the control handle:

```go
turns := 0
for event, err := range stream {
	if err != nil { log.Fatal(err) }
	if _, ok := event.(*agents.AgentUpdatedStreamEvent); ok {
		if turns++; turns >= maxUserTurns {
			ctrl.StopAfterTurn() // let this turn finish, then stop
		}
	}
	// … handle event …
}
```

## Semantics

- A failing run ends with a non-nil error from the iterator and emits **no**
  `RunCompletedEvent`. There is nowhere else to look — the error cannot reach
  one place and be lost from another.
- Everything else works as in `RunSync`: sessions are saved per turn, guardrails
  fire, tracing records the same spans. Input guardrails race the first model
  call in both entry points; set `Blocking: true` to gate instead
  ([Guardrails](guardrails.md)).
- A run can pause for [tool approval](human_in_the_loop.md): range to the end,
  read `Interruptions` / `State` off the result, and resume with
  `agents.ResumeRun` — which returns a stream of its own, so the continuation
  streams like the original. The resumed stream does not re-emit the paused
  turn's items; it picks up with the approved tools' outputs.

## Fanning out to many consumers

One run, several consumers reading at different speeds — use `agents.Fanout`. It
buffers per subscriber, so a slow one cannot stall the run or its peers, and it
**reports** what it had to drop rather than dropping silently:

```go
f := agents.NewFanout[agents.StreamEvent](agents.FanoutOptions{Replay: 512, Subscriber: 512})
go func() {
	defer f.Close()
	for ev, err := range stream {
		if err != nil {
			return
		}
		f.Publish(ev)
	}
}()

sub, cancel := f.Subscribe(0)
defer cancel()
for item, err := range sub {
	var gap *agents.GapError
	if errors.As(err, &gap) {
		// This subscriber fell behind. Resync from gap.LastGood.
	}
	render(item.Value)
}
```
