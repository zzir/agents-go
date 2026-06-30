// Command sandbox shows an agent that writes, reads and runs code in a sandbox.
//
// This example uses sandbox.NewLocalWithOptions with a temporary WorkDir, which
// runs code on the host WITHOUT isolation — fine for a trusted local demo, but
// never for untrusted code in production. To isolate execution, swap in the
// Docker backend (a separate module so this example stays dependency-light):
//
//	import "github.com/zzir/agents-go/sandbox/docker"
//	sb, _ := docker.New(docker.Options{
//		Image:      "python:3.12-slim",
//		Persistent: true,
//		Limits:     sandbox.Limits{MemoryBytes: 256 << 20, CPUs: 0.5},
//	})
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
	defer os.RemoveAll(workDir)

	// Dev-only, unisolated backend. See the package doc above for Docker.
	sb := sandbox.NewLocalWithOptions(sandbox.LocalOptions{WorkDir: workDir})
	defer sb.Close()

	// exec_command runs shell commands; read_file / write_file / list_files
	// give the agent native file I/O (no shell needed).
	tools := []agents.Tool{
		sandbox.CodeTool(sb, sandbox.CodeToolConfig{
			Name:        "exec_command",
			Description: "Execute a shell command and return its stdout, stderr and exit code.",
		}),
	}
	tools = append(tools, sandbox.FileTools(sb, sandbox.FileToolConfig{})...)

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

	res, err := agents.Run(context.Background(), agent,
		"用 Python 计算第 20 个斐波那契数,并打印结果。", agents.RunOptions{
			ModelProvider: openai.NewProvider(),
		})
	if err != nil {
		return err
	}
	fmt.Println(res.FinalOutputString())
	return nil
}
