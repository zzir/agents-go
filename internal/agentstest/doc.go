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
//	agent := &agents.Agent{Name: "a", Tools: []*agents.Tool{weatherTool}, ModelImpl: model}
//	res, err := agents.RunSync(ctx, agent, "weather in SF?", agents.RunOptions{})
//
// Each turn of the builder becomes one model response, so the example above
// scripts a run that calls a tool and then answers.
//
// The package also carries assertion helpers ([AssertFinalOutput],
// [ToolCallNames], …) and item constructors ([MessageItem], [RawItem], …) for
// tests that need to assemble responses by hand.
//
// It is internal to this repository; the agents package cannot import it (an
// import cycle) and keeps its own unexported fakes.
package agentstest
