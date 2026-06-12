// Command sandbox shows an agent that writes and runs code in a sandbox.
//
// This example uses sandbox.NewLocal(), which runs code on the host WITHOUT
// isolation — fine for a trusted local demo, but never for untrusted code in
// production. To isolate execution, swap in the Docker or Kubernetes backend
// (separate modules so this example stays dependency-light):
//
//	import "github.com/zzir/agents-go/sandbox/docker"
//	sb, _ := docker.New(docker.Options{
//		Image:  "python:3.12-slim",
//		Limits: sandbox.Limits{MemoryBytes: 256 << 20, CPUs: 0.5},
//	})
//	// ...with the python image, set RunCmd to []string{"python", "main.py"}.
//
//	import "github.com/zzir/agents-go/sandbox/k8s"
//	sb, _ := k8s.New(k8s.Options{Image: "python:3.12-slim", Namespace: "sandboxes"})
//
// Run with: OPENAI_API_KEY=... go run ./examples/sandbox   (host needs python3)
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/openai"
	"github.com/zzir/agents-go/sandbox"
)

func main() {
	// Dev-only, unisolated backend. See the package doc above for Docker/K8s.
	sb := sandbox.NewLocal()
	defer sb.Close()

	runPython := sandbox.CodeTool(sb, sandbox.CodeToolConfig{
		Name:        "run_python",
		Description: "Execute Python 3 code and return its stdout, stderr and exit code.",
		Filename:    "main.py",
		RunCmd:      []string{"python3", "main.py"},
	})

	agent := &agents.Agent{
		Name:         "coder",
		Instructions: agents.StaticInstructions("Solve computational problems by writing and running Python with the run_python tool. Print the answer."),
		Model:        "gpt-4o",
		Tools:        []agents.Tool{runPython},
	}

	res, err := agents.Run(context.Background(), agent,
		"用 Python 计算第 20 个斐波那契数,并打印结果。", agents.RunOptions{
			ModelProvider: openai.NewProvider(),
		})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.FinalOutputString())
}
