// Command e2b runs the same coding agent as examples/sandbox, but inside an
// E2B-compatible cloud sandbox instead of the unisolated local backend — real
// isolation for untrusted model code, with the identical tool wiring.
//
// sandbox/e2b talks to any service speaking the E2B API: E2B's own cloud, a
// self-hosted E2B, or a compatible service such as Alibaba Cloud's Function
// Compute cloud sandbox. The remote sandbox is provisioned LAZILY on the first
// tool call (or eagerly via sb.Start(ctx)); OnSandboxID is how a long-lived
// app remembers the sandbox it created, so a restart resumes that one rather
// than provisioning — and billing for — a second.
//
// Cleanup is the gotcha: Close() releases nothing remote (the sandbox outlives
// the client), so this example calls sb.Destroy(ctx) to tear the remote
// sandbox and its filesystem down — otherwise it leaks a billed sandbox.
//
// Run with:
//
//	OPENAI_API_KEY=... E2B_API_KEY=... E2B_TEMPLATE_ID=base go run ./examples/e2b
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/models/openai"
	"github.com/zzir/agents-go/sandbox"
	"github.com/zzir/agents-go/sandbox/e2b"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

// run keeps the deferred sandbox teardown ahead of any fatal exit.
func run() error {
	ctx := context.Background()

	apiKey := os.Getenv("E2B_API_KEY")
	if apiKey == "" {
		return errors.New("E2B_API_KEY is required (get a key at https://e2b.dev)")
	}
	// A stock E2B template; the working directory is made on the sandbox, not
	// expected of the image. Point at your own template for other services.
	templateID := os.Getenv("E2B_TEMPLATE_ID")
	if templateID == "" {
		templateID = "base"
	}

	// New does no I/O; the remote sandbox appears on first use. AllowInternet is
	// off — this task needs no network, and the E2B sandbox joins none by default.
	sb, err := e2b.New(e2b.Options{APIKey: apiKey, TemplateID: templateID, AllowInternet: false})
	if err != nil {
		return err
	}
	// Close() ≠ Destroy() for e2b: Close frees nothing remote. Tear the sandbox
	// down explicitly (best-effort) or leak a billed one.
	defer func() {
		if err := sb.Destroy(ctx); err != nil {
			log.Printf("e2b: destroy sandbox: %v", err)
		}
	}()

	// exec_command runs shell commands; read_file / write_file / list_files give
	// the agent native file I/O; apply_patch applies Codex-style multi-file
	// patches. All of them edit through the same Sandbox, so they share the
	// filesystem exec_command runs in — here, /workspace on the cloud sandbox.
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

	res, err := agents.RunSync(ctx, agent,
		"用 Python 计算第 20 个斐波那契数,并打印结果。", agents.RunOptions{Model: agents.ModelOptions{Provider: openai.NewProvider()}})
	if err != nil {
		return err
	}
	fmt.Println(res.FinalOutputString())
	return nil
}
