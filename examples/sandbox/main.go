// Command sandbox shows an agent that writes, reads and runs code in a sandbox.
// The local backend runs on the host with NO isolation; swap in sandbox/docker
// (its own module) for untrusted code — see docs/howto/sandbox.md.
//
// Run with: OPENAI_API_KEY=... go run ./examples/sandbox   (host needs python3)
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/openai"
	"github.com/zzir/agents-go/sandbox"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

// run keeps the deferred sandbox cleanup ahead of any fatal exit.
func run() error {
	workDir, err := os.MkdirTemp("", "sandbox-example-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(workDir) }()

	// Host-local, unisolated. MaxReadFileBytes caps one read_file (0 = the 8
	// MiB default).
	sb := sandbox.NewLocalWithOptions(sandbox.LocalOptions{WorkDir: workDir, MaxReadFileBytes: 1 << 20})
	defer sb.Close()

	// exec_command, the file tools and apply_patch all share the sandbox filesystem.
	tools := []*agents.Tool{
		sandbox.CodeTool(sb, sandbox.CodeToolConfig{
			Name:        "exec_command",
			Description: "Execute a shell command and return its stdout, stderr and exit code.",
		}),
	}
	tools = append(tools, sandbox.FileTools(sb, sandbox.FileToolConfig{})...)
	tools = append(tools, sandbox.ApplyPatchTool(sb, sandbox.FileToolConfig{}))

	agent := &agents.Agent{
		Name: "coder",
		Instructions: agents.StaticInstructions(
			"Solve computational problems by writing Python scripts with write_file, " +
				"running them with exec_command, and reading any output files with read_file. " +
				"Use list_files to inspect the working directory when needed. Print the answer.",
		),
		Model: "gpt-4o",
		Tools: tools,
	}

	res, err := agents.RunSync(context.Background(), agent,
		"用 Python 计算第 20 个斐波那契数,并打印结果。", agents.RunOptions{Model: agents.ModelOptions{Provider: openai.NewProvider()}})
	if err != nil {
		return err
	}
	fmt.Println(res.FinalOutputString())
	return nil
}
