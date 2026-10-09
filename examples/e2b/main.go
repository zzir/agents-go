// Command e2b runs the coding agent of examples/sandbox inside an E2B-compatible
// cloud sandbox: real isolation, identical tool wiring. The remote sandbox is
// provisioned lazily; OnSandboxID lets a restart resume it — see docs/howto/sandbox.md.
//
// Run with:
//
//	OPENAI_API_KEY=... E2B_API_KEY=... E2B_TEMPLATE_ID=base go run ./examples/e2b
package main

import (
	"context"
	"encoding/json"
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
	// Any template works: the working directory is made on the sandbox — see
	// spec §2.7q.
	templateID := os.Getenv("E2B_TEMPLATE_ID")
	if templateID == "" {
		templateID = "base"
	}

	// New does no I/O. AllowInternet stays off (the task needs none). A
	// compatible service's own auth header goes in
	// E2B_HEADERS='{"Authorization": "Bearer <key>"}'.
	var headers map[string]string
	if raw := os.Getenv("E2B_HEADERS"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &headers); err != nil {
			return fmt.Errorf("E2B_HEADERS: %w", err)
		}
	}
	sb, err := e2b.New(e2b.Options{
		APIURL:        os.Getenv("E2B_API_URL"),
		Domain:        os.Getenv("E2B_DOMAIN"),
		APIKey:        apiKey,
		Headers:       headers,
		TemplateID:    templateID,
		AllowInternet: false,
	})
	if err != nil {
		return err
	}
	// Close frees nothing remote; Destroy tears the billed sandbox down.
	defer func() {
		if err := sb.Destroy(ctx); err != nil {
			log.Printf("e2b: destroy sandbox: %v", err)
		}
	}()

	// exec_command, the file tools and apply_patch share the sandbox filesystem
	// (/workspace).
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
