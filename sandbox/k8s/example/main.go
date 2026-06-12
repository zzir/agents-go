// Command example runs an agent whose code tool executes as a Kubernetes Job.
//
// Prerequisites: a kubeconfig (or in-cluster config) with permission to create
// Jobs, Pods and ConfigMaps in the target namespace, and the image available to
// the cluster.
//
// Run from the k8s module directory:
//
//	cd sandbox/k8s && OPENAI_API_KEY=... go run ./example
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/openai"
	"github.com/zzir/agents-go/sandbox"
	"github.com/zzir/agents-go/sandbox/k8s"
)

func main() {
	sb, err := k8s.New(k8s.Options{
		Image:     "python:3.12-slim",
		Namespace: "default",
		Limits:    sandbox.Limits{MemoryBytes: 256 << 20, CPUs: 0.5},
		// Each call runs as a one-shot Job: non-root, read-only root fs, dropped
		// capabilities, no service-account token, with an activeDeadline.
	})
	if err != nil {
		log.Fatal(err)
	}
	defer sb.Close()

	runPython := sandbox.CodeTool(sb, sandbox.CodeToolConfig{
		Name:        "run_python",
		Description: "Execute Python 3 code as a Kubernetes Job and return its output.",
		Filename:    "main.py",
		RunCmd:      []string{"python", "main.py"},
	})

	agent := &agents.Agent{
		Name:         "coder",
		Instructions: agents.StaticInstructions("Solve problems by writing and running Python with run_python. Print the answer."),
		Model:        "gpt-5.5",
		Tools:        []agents.Tool{runPython},
	}

	res, err := agents.Run(context.Background(), agent,
		"用 Python 计算第 30 个斐波那契数,并打印结果。", agents.RunOptions{
			ModelProvider: openai.NewProvider(),
		})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.FinalOutputString())
}
