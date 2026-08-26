# Testing your agents

An agent's behavior is mostly your code — which tools it has, what its
instructions say, what it does with a tool result. None of that needs a real
model to test, and testing it against one gives you a suite that is slow, costs
money, and fails for reasons unrelated to your change.

The seam is the `agents.Model` interface: fake the model, run everything else
for real. Your tools actually execute; what is faked is only the decision to
call them.

## A scripted model

`Model` has two methods, and a test double only needs the one its tests reach —
`agents.RunSync` calls `Respond`, the streaming entry point calls
`StreamResponse`:

```go
type scriptedModel struct {
	responses []*agents.ModelResponse // one per turn
	calls     int
}

func (m *scriptedModel) Respond(ctx context.Context, req agents.ModelRequest) (*agents.ModelResponse, error) {
	res := m.responses[m.calls]
	m.calls++
	return res, nil
}

func (m *scriptedModel) StreamResponse(ctx context.Context, req agents.ModelRequest) iter.Seq2[*agents.ResponseStreamEvent, error] {
	panic("this test suite only runs RunSync")
}
```

A turn's `ModelResponse` holds Responses-API output items: an assistant message
ends the run with that text as final output; a function-call item makes the run
execute your tool for real and feed its return value to the next scripted turn.
So "call the tool, then answer" is a two-response script.

Point the run at it with `ModelOptions.Override` and no provider is ever
consulted:

```go
res, err := agents.RunSync(context.Background(), newAgent(), "the answer?",
	agents.RunOptions{Model: agents.ModelOptions{Override: model}})
```

`Override` replaces the model for **every** agent in the run, which is what you
usually want in a test — a handoff target gets the same scripted model without
being wired up separately. To script one agent only, set `Agent.ModelImpl`
instead.

Worth asserting beyond the final output: that the script was fully consumed (a
run that stopped earlier than the test assumed is exactly what a test should
catch), and — for order-sensitive tests — the kinds of `res.NewItems` in
sequence.

## When not to fake the model

A fake model stands in for the model. It does not stand in for anything else,
on purpose — a test that stubs out the run loop is testing the stub.

- **Testing a tool in isolation?** Call it directly. A `Tool`'s
  function is an ordinary Go function; you do not need a run to exercise it.
- **Testing against the real API?** Use a real provider and mark the test so it
  can be skipped without a key. Keep those few and separate from the fast suite.
- **Writing a `Model` implementation?** Run it through
  `modelkit/conformancetest` — the golden matrix every in-repo backend passes —
  plus tests against the provider's wire format. A scripted fake is a consumer
  of the `Model` interface, not a conformance suite for it.

The worked version of this page is [examples/testing](../../examples/testing) — a scripted model, the agent's real tool, and a test that asserts both the answer and that the script was fully consumed. It needs no API key: `go test ./examples/testing`.
