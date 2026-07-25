// Package agentstest provides test doubles for code built on the agents SDK.
//
// The centerpiece is [FakeModel], a scripted [agents.Model] that returns queued
// responses instead of calling a provider. Build one with [NewResponseBuilder]:
//
//	model := agentstest.NewResponseBuilder().
//	    FunctionCall("get_weather", "call_1", `{"city":"SF"}`).
//	    NewTurn().
//	    Text("it is sunny").
//	    Build()
//
//	agent := &agents.Agent{Name: "a", Tools: []agents.Tool{weatherTool}, ModelImpl: model}
//	res, err := agents.Run(ctx, agent, "weather in SF?", agents.RunOptions{})
//
// Each turn of the builder becomes one model response, so the example above
// scripts a run that calls a tool and then answers.
//
// The package also carries assertion helpers ([AssertFinalOutput],
// [ToolCallNames], …) and item constructors ([MessageItem], [RawItem], …) for
// tests that need to assemble responses by hand.
//
// It is part of the root module and pulls in no dependencies beyond the SDK
// itself, so importing it from a test binary costs nothing at runtime.
//
// # Scope
//
// agentstest is to this SDK what net/http/httptest is to net/http: a public
// harness for code that *uses* the package. The agents package's own internal
// tests cannot import it — that would be an import cycle — and keep their own
// unexported fakes, which they need anyway to reach unexported behavior.
// Everything outside package agents (submodules, examples, and your code) can
// use it.
package agentstest
