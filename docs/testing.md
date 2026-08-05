# Testing your agents

An agent's behavior is mostly your code — which tools it has, what its
instructions say, what it does with a tool result. None of that needs a real
model to test, and testing it against one gives you a suite that is slow, costs
money, and fails for reasons unrelated to your change.

`agentstest` is the harness for that: a scripted `agents.Model` plus the
assertions that go with it. It is what `net/http/httptest` is to `net/http` — a
public test double for code that *uses* the package.

```go
import "github.com/zzir/agents-go/agentstest"
```

It is part of the root module and adds no dependencies, so importing it from a
test binary costs nothing at runtime.

A complete, runnable version of everything below is in
[`examples/testing`](../examples/testing) — the agent in `main.go`, its tests in
`agent_test.go`, which is the layout to copy.

## The smallest test

`TextModel` answers with one string per turn. Point the run at it with
`ModelOptions.Override` and no provider is ever consulted:

```go
func TestAgentAnswers(t *testing.T) {
	model := agentstest.TextModel("42")

	res, err := agents.RunSync(context.Background(), newAgent(), "the answer?",
		agents.RunOptions{Model: agents.ModelOptions{Override: model}})
	if err != nil {
		t.Fatal(err)
	}
	agentstest.AssertFinalOutput(t, res, "42")
}
```

`Override` replaces the model for **every** agent in the run, which is what you
usually want in a test — a handoff target gets the same scripted model without
being wired up separately. To script one agent only, set `Agent.ModelImpl`
instead.

## Scripting a tool call

A tool call and the answer that follows it are **two model calls**, so a script
that exercises a tool has two turns. `NewTurn` ends one and starts the next:

```go
model := agentstest.NewResponseBuilder().
	Reasoning("the user wants weather").
	FunctionCall("get_weather", "call_1", `{"city":"SF"}`).
	NewTurn().
	Text("It's sunny and 23°C in SF.").
	Build()
```

The run this drives calls `get_weather` with `{"city":"SF"}`, feeds your tool's
real return value back to the model, and then produces the second turn's text as
its final output. **Your tool actually runs** — that is the point. What is faked
is only the decision to call it.

Pass invalid JSON to `FunctionCall` to exercise your tool's argument-parsing
error path, and `Raw` to emit an item type the SDK does not model.

## Asserting

| Helper | Checks |
|---|---|
| `AssertFinalOutput(t, res, want)` | the run's final output |
| `AssertModelCalls(t, model, n)` | how many turns the run actually took |
| `AssertScriptExhausted(t, model)` | nothing scripted went unused |
| `ToolCallNames(res.NewItems)` | which tools the model asked for, in order |
| `MessageTexts(res.NewItems)` | every assistant message, in order |
| `ItemTypes(res.NewItems)` | the item sequence, for order-sensitive tests |

`AssertScriptExhausted` is the one worth reaching for by default. A script
longer than the run means the agent stopped earlier than the test assumed —
which is exactly the kind of change a test should catch and an assertion on the
final output alone will not.

## Streaming

`CollectRun` drives a stream to completion and hands back both halves, so one
test can assert on the sequence a UI would render as well as on the answer:

```go
stream, _ := agents.Run(ctx, newAgent(), "weather in SF?",
	agents.RunOptions{Model: agents.ModelOptions{Override: model}})
events, res := agentstest.CollectRun(t, stream)

agentstest.AssertFinalOutput(t, res, "sunny")
names := agentstest.RunItemEventNames(events) // tool_called, tool_output, message_output_created
```

Use `CollectEvents` when the result does not matter. Set
`FakeModel.StreamTextDeltas` to emit per-character deltas if what you are
testing is delta handling; it is off by default because most tests want the
assembled response and deltas make a transcript hard to read.

## Failure paths

`Fail` makes a turn return an error instead of a response, which is how you test
that your program surfaces a model failure rather than answering with something
invented:

```go
boom := errors.New("upstream is down")
model := agentstest.NewResponseBuilder().Fail(boom).Build()

_, err := agents.RunSync(ctx, newAgent(), "weather?",
	agents.RunOptions{Model: agents.ModelOptions{Override: model}})
if !errors.Is(err, boom) {
	t.Fatalf("err = %v, want it to wrap %v", err, boom)
}
```

This composes with [error handlers](running_agents.md#error-handlers): script
the failure, then assert that the fallback output arrived rather than an error.

## When not to use it

`agentstest` fakes the model. It does not fake anything else, on purpose — a
test that stubs out the run loop is testing the stub.

- **Testing a tool in isolation?** Call it directly. A `Tool`'s
  function is an ordinary Go function; you do not need a run to exercise it.
- **Testing against the real API?** Use a real provider and mark the test so it
  can be skipped without a key. Keep those few and separate from the fast suite.
- **Writing a `Model` implementation?** Run it through
  `modelkit/conformancetest` — the golden matrix every in-repo backend passes —
  plus tests against the provider's wire format. `FakeModel` is a consumer of
  the `Model` interface, not a conformance suite for it.
