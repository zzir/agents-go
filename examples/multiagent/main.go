// Command multiagent shows agent-as-tool orchestration: a manager agent calls
// specialists as tools and keeps control, which is the alternative to a handoff
// (where control moves and does not come back). See examples/handoffs for that.
//
// Run with: OPENAI_API_KEY=... go run ./examples/multiagent
package main

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/openai"
)

func main() {
	provider := openai.NewProvider()

	translator := &agents.Agent{
		Name:         "translator",
		Instructions: agents.StaticInstructions("Translate the text you are given into French. Reply with the translation only."),
		Model:        "gpt-4o-mini",
	}

	summarizer := &agents.Agent{
		Name:         "summarizer",
		Instructions: agents.StaticInstructions("Summarize the text you are given in one sentence."),
		Model:        "gpt-4o-mini",
	}

	// AsTool wraps a whole run as a callable tool. The nested run gets its own
	// turns, its own tools and its own spans; the manager sees one tool result.
	manager := &agents.Agent{
		Name: "manager",
		Instructions: agents.StaticInstructions(
			"You coordinate specialists. Summarize first, then translate the summary. " +
				"Report both, labelled."),
		Model: "gpt-4o",
		Tools: []*agents.Tool{
			summarizer.AsTool(agents.AgentToolConfig{
				Name:        "summarize",
				Description: "Summarize a piece of text in one sentence.",
			}),
			translator.AsTool(agents.AgentToolConfig{
				Name:        "translate_to_french",
				Description: "Translate a piece of text into French.",
				// The nested run's final output is the tool result by default;
				// an extractor reshapes it without touching the child agent.
				CustomOutputExtractor: func(r *agents.RunResult) (string, error) {
					return strings.TrimSpace(r.FinalOutputString()), nil
				},
			}),
		},
	}

	const article = `Go's garbage collector is a concurrent, tri-color mark-sweep collector.
It runs alongside the program rather than stopping it, trading a little throughput
for pause times that stay in the sub-millisecond range even on large heaps.`

	res, err := agents.RunSync(context.Background(), manager, article,
		agents.RunOptions{Model: agents.ModelOptions{Provider: provider}})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.FinalOutputString())

	// Usage rolls up: a nested run's tokens are attributed to the parent run.
	u := res.Usage
	fmt.Printf("\n%d requests, %d input + %d output tokens (nested runs included)\n",
		u.Requests, u.InputTokens, u.OutputTokens)
}
