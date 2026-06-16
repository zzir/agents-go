// Command example wires Agent Skills into an agent: it loads a skills directory,
// injects the skill index into the agent's instructions (discovery), and gives
// the model a read_skill_file tool to open SKILL.md bodies on demand
// (activation/execution).
//
// Run from the skills module directory:
//
//	cd skills && OPENAI_API_KEY=... go run ./example
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/openai"
	"github.com/zzir/agents-go/skills"
)

func main() {
	const dir = "./example/skills"

	loaded, err := skills.Load(dir)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("loaded %d skill(s)", len(loaded))

	agent := &agents.Agent{
		Name: "assistant",
		Instructions: agents.InstructionsFunc(func(_ context.Context, _ *agents.RunContext, _ *agents.Agent) (string, error) {
			return "You are a helpful assistant.\n\n" + skills.RenderIndex(loaded), nil
		}),
		Model: "gpt-4o",
		Tools: []agents.Tool{skills.ReadFileTool(dir)},
	}

	res, err := agents.Run(context.Background(), agent,
		"Summarize a PDF for me — what process should you follow?", agents.RunOptions{
			ModelProvider: openai.NewProvider(),
		})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.FinalOutputString())
}
