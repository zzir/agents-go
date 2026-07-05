// Command example runs an agent whose code tool executes in a Docker sandbox.
//
// Prerequisites: a reachable Docker daemon and the image pulled locally
// (docker pull python:3.12-slim).
//
// Run from the docker module directory:
//
//	cd sandbox/docker && OPENAI_API_KEY=... go run ./example
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/openai"
	"github.com/zzir/agents-go/sandbox"
	"github.com/zzir/agents-go/sandbox/docker"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	sb, err := docker.New(docker.Options{
		Image:  "python:3.12-slim",
		Limits: sandbox.Limits{MemoryBytes: 256 << 20, CPUs: 0.5}, // 256 MiB, half a core
		// Network defaults to off; root fs is read-only; runs as nobody.
	})
	if err != nil {
		return err
	}
	defer sb.Close()

	runPython := sandbox.CodeTool(sb, sandbox.CodeToolConfig{
		Name:        "exec_command",
		Description: "Execute a shell command in an isolated container and return its output.",
	})

	agent := &agents.Agent{
		Name:         "coder",
		Instructions: agents.StaticInstructions("Solve problems by writing and running Python with run_python. Print the answer."),
		Model:        "gpt-4o",
		Tools:        []agents.Tool{runPython},
	}

	res, err := agents.Run(context.Background(), agent,
		"用 Python 计算 1 到 100 的素数个数,并打印结果。", agents.RunOptions{
			ModelProvider: openai.NewProvider(),
		})
	if err != nil {
		return err
	}
	fmt.Println(res.FinalOutputString())
	return nil
}
