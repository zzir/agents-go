# Streaming

`agents.RunStreamed` runs the same agent loop as `Run` but emits events while it executes — raw model deltas, completed run items, and agent switches.

```go
sr := agents.RunStreamed(ctx, agent, "Tell me 5 jokes.", agents.RunOptions{ModelProvider: provider})

for event, err := range sr.Events() {
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
	}
}

res, err := sr.FinalResult() // the completed RunResult, same as a non-streamed run
```

`Events()` is an `iter.Seq2[StreamEvent, error]` — Go's range-over-func replaces Python's `async for`.

## Event types

### Raw response events

`*RawResponsesStreamEvent` passes through every OpenAI Responses streaming event (`response.created`, `response.output_text.delta`, …). Use these for token-level UI streaming.

### Run item events and agent events

`*RunItemStreamEvent` fires when an item is **complete** (a full message, a tool call, a tool result) — the right granularity for "Fetching the weather…"-style progress, ignoring per-token noise. Event names mirror Python: `message_output_created`, `tool_called`, `tool_output`, `handoff_requested`, `handoff_occured`, `reasoning_item_created`.

`*AgentUpdatedStreamEvent` fires when a handoff switches the active agent.

## Controlling and inspecting a running stream

The `*StreamedResult` returned by `RunStreamed` exposes three helpers you can call from the event-consuming goroutine while the run is still in flight:

- `sr.StopAfterTurn()` requests a **graceful** stop: the in-flight turn finishes — including its tool calls and session save — and then the run stops cleanly before the next turn begins, with no error and a nil `FinalOutput`. It is the counterpart of Python's `StreamedResult.cancel(mode="after_turn")`; to stop *immediately* instead, cancel the run's context (Python's "immediate" mode).
- `sr.CurrentAgent()` returns the agent handling the turn in progress, or nil before the first turn starts.
- `sr.CurrentTurn()` returns the 1-based number of the turn in progress, or 0 before the first turn starts.

```go
for event, err := range sr.Events() {
	if err != nil { log.Fatal(err) }
	if sr.CurrentTurn() >= maxUserTurns {
		sr.StopAfterTurn() // let this turn finish, then stop
	}
	// … handle event …
}
```

## Semantics

- Events are delivered on a buffered channel; the producing goroutine blocks when you stop consuming. **If you break out of the loop early, cancel the run's context** — otherwise the run goroutine leaks waiting to deliver.
- A failing run yields one terminal error from the iterator, and `FinalResult()` returns the same error.
- Everything else works as in `Run`: sessions are saved on completion, guardrails fire (input guardrails run synchronously before the first model call in streamed mode), tracing records the same spans.
- A streamed run can pause for [tool approval](human_in_the_loop.md): iterate to the end, then check `FinalResult()`'s `Interruptions`/`State` and resume with `agents.ResumeRun` (the resumed run is non-streamed).
