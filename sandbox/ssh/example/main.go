// Command example runs an agent whose code tool executes on a remote host over
// SSH. Files are transferred via SFTP and the command runs in a throwaway
// directory.
//
// The SSH backend provides NO isolation and does not enforce resource limits;
// point it at a disposable VM or an already-sandboxed host.
//
// Run from the ssh module directory:
//
//	cd sandbox/ssh && \
//	  SSH_HOST=dev-box SSH_USER=sandbox SSH_KEY=~/.ssh/id_ed25519 \
//	  OPENAI_API_KEY=... go run ./example
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
	sshsb "github.com/zzir/agents-go/sandbox/ssh"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	host := os.Getenv("SSH_HOST")
	user := os.Getenv("SSH_USER")
	if host == "" || user == "" {
		return errors.New("set SSH_HOST and SSH_USER")
	}

	sb, err := sshsb.New(sshsb.Options{
		Addr: host,
		User: user,
		Auth: sshsb.AuthConfig{
			KeyFile:  os.Getenv("SSH_KEY"), // optional
			Password: os.Getenv("SSH_PASS"),
		},
		// Verifies against ~/.ssh/known_hosts by default. For a throwaway dev
		// box without a known_hosts entry, set:
		//   HostKey: sshsb.HostKeyConfig{InsecureIgnoreHostKey: true},
	})
	if err != nil {
		return err
	}
	defer sb.Close()

	runPython := sandbox.CodeTool(sb, sandbox.CodeToolConfig{
		Name:        "exec_command",
		Description: "Execute a shell command on the remote host and return its output.",
	})

	agent := &agents.Agent{
		Name:         "coder",
		Instructions: agents.StaticInstructions("Solve problems by writing and running Python with run_python. Print the answer."),
		Model:        "gpt-4o",
		Tools:        []agents.Tool{runPython},
	}

	res, err := agents.RunSync(context.Background(), agent,
		"用 Python 计算 1 到 100 的素数个数,并打印结果。", agents.RunOptions{
			ModelProvider: openai.NewProvider(),
		})
	if err != nil {
		return err
	}
	fmt.Println(res.FinalOutputString())
	return nil
}
