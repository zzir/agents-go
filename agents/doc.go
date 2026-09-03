// Package agents is a Go SDK for building agent applications on the OpenAI
// Responses API: a run loop with tools, handoffs, guardrails, sessions,
// human-in-the-loop approval and tracing.
//
//	agent := &agents.Agent{
//		Name:         "assistant",
//		Instructions: agents.StaticInstructions("Be brief."),
//	}
//	res, err := agents.RunSync(ctx, agent, "hello", agents.RunOptions{
//		Model: agents.ModelOptions{Provider: openai.NewProvider()},
//	})
//
// Behavior: docs/reference/spec.md. Layout: docs/explanation/architecture.md.
// How-to guides: docs/howto/.
package agents
