// Command hitl demonstrates human-in-the-loop tool approval: the run pauses on a
// tool that needs approval, the user decides, then the run resumes.
//
// Run with: OPENAI_API_KEY=... go run ./examples/hitl
package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/openai"
)

func main() {
	deleteFile := agents.NewFunctionTool("delete_file", "Delete a file from disk.",
		func(ctx context.Context, tc *agents.ToolContext, args struct {
			Path string `json:"path" jsonschema:"path of the file to delete"`
		}) (string, error) {
			return "deleted " + args.Path, nil
		})
	deleteFile.NeedsApproval = true // pause before running

	agent := &agents.Agent{
		Name:         "ops",
		Instructions: agents.StaticInstructions("Help with file operations."),
		Model:        "gpt-4o",
		Tools:        []agents.Tool{deleteFile},
	}

	ctx := context.Background()
	opts := agents.RunOptions{Model: agents.ModelOptions{Provider: openai.NewProvider()}}

	res, err := agents.RunSync(ctx, agent, "请删除 /tmp/old.log 文件。", opts)
	if err != nil {
		log.Fatal(err)
	}

	// Resolve any approval interruptions, then resume.
	for len(res.Interruptions) > 0 {
		for _, item := range res.Interruptions {
			fmt.Printf("Approve tool %q with args %s ? [y/N] ", item.ToolName, item.Arguments)
			reply, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			if strings.TrimSpace(strings.ToLower(reply)) == "y" {
				res.State.Approve(item, false)
			} else {
				res.State.Reject(item, false, "User denied the operation.")
			}
		}
		res, err = agents.ResumeRunSync(ctx, res.State, opts)
		if err != nil {
			log.Fatal(err)
		}
	}

	fmt.Println(res.FinalOutputString())
}
