// Command example wires Agent Skills into an agent: it reads SKILL.md
// documents from disk, validates them with skills.Parse, injects the skill
// index into the agent's instructions (discovery), and gives the model a
// read_skill tool to open a skill's full document on demand
// (activation/execution). Storage is the caller's: here a map from skill name
// to document content.
//
// Run from the skills module directory:
//
//	cd skills && OPENAI_API_KEY=... go run ./example
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/openai"
	"github.com/zzir/agents-go/skills"
)

type readSkillArgs struct {
	Name string `json:"name" jsonschema:"the skill's name from the index, e.g. pdf-summary"`
}

func main() {
	const dir = "./example/skills"

	// The caller owns storage: read each skill directory's SKILL.md, validate
	// it, and keep the content by name for the read_skill tool.
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Fatal(err)
	}
	var index []skills.Skill
	content := map[string]string{}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name(), "SKILL.md"))
		if err != nil {
			continue
		}
		sk, err := skills.Parse(data)
		if err != nil {
			log.Fatalf("%s: %v", e.Name(), err)
		}
		index = append(index, sk)
		content[sk.Name] = string(data)
	}
	log.Printf("loaded %d skill(s)", len(index))

	readSkill := agents.NewTool("read_skill",
		"Read a skill's full SKILL.md instructions by name.",
		func(_ context.Context, _ *agents.ToolContext, args readSkillArgs) (string, error) {
			doc, ok := content[args.Name]
			if !ok {
				return "", fmt.Errorf("no skill named %q", args.Name)
			}
			return doc, nil
		})

	agent := &agents.Agent{
		Name: "assistant",
		Instructions: func(_ context.Context, _ *agents.RunContext, _ *agents.Agent) (string, error) {
			return "You are a helpful assistant.\n\n" + skills.RenderIndex(index), nil
		},
		Model: "gpt-4o",
		Tools: []*agents.Tool{readSkill},
	}

	res, err := agents.RunSync(context.Background(), agent,
		"Summarize a PDF for me — what process should you follow?", agents.RunOptions{Model: agents.ModelOptions{Provider: openai.NewProvider()}})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.FinalOutputString())
}
