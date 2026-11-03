// Command exec_background runs a shell command in the background: exec_command
// with background: true hands the approved command to the host, which makes it
// a task whose launcher runs it and whose ending wakes the parent — see spec
// §2.7w and §2.13, and docs/howto/sandbox.md.
//
// Run with: OPENAI_API_KEY=... go run ./examples/exec_background
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/agents/tasks"
	"github.com/zzir/agents-go/models/openai"
	"github.com/zzir/agents-go/sandbox"
)

// kindCommand is this host's name for a task that is one shell command.
const kindCommand = "command"

// commandState is what the task row keeps about its command: the call id
// under which the host parked the *sandbox.Command.
type commandState struct {
	CallID string `json:"call_id"`
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx := context.Background()
	provider := openai.NewProvider() // reads OPENAI_API_KEY

	workDir, err := os.MkdirTemp("", "exec-background-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(workDir) }()
	sb := sandbox.NewLocalWithOptions(sandbox.LocalOptions{WorkDir: workDir})
	defer sb.Close()

	repo := session.NewInMemoryRepo()
	store := tasks.NewInMemoryStore()
	var (
		mu      sync.Mutex
		pending = map[string]*sandbox.Command{} // call id → the command to run
		wg      sync.WaitGroup
		mgr     *tasks.Manager
	)

	mgr = tasks.New(tasks.Config{
		Store:    store,
		Sessions: repo,
		// A command task has no agent; the Resolver only names it.
		Resolver: tasks.AgentResolver(func(context.Context, string, string) (tasks.Spec, error) {
			return tasks.Spec{DisplayName: "command"}, nil
		}),
		// A command cannot be resumed from where it stopped: its ceiling is 1.
		MaxAttemptsFor: func(t *tasks.Task) int {
			if t.Kind == kindCommand {
				return 1
			}
			return 0
		},
		// The launcher takes the parked command and runs it on its own goroutine.
		Launcher: tasks.Launcher(func(_ context.Context, req tasks.LaunchRequest) error {
			var st commandState
			_ = json.Unmarshal(req.State, &st)
			mu.Lock()
			cmd := pending[st.CallID]
			delete(pending, st.CallID)
			mu.Unlock()
			if cmd == nil {
				return fmt.Errorf("no command parked for call %q", st.CallID)
			}
			wg.Go(func() {
				out := tasks.RunOutcome{RunID: req.RunID, Status: tasks.StatusCompleted}
				res, err := cmd.Run(context.Background(), sb)
				if err != nil {
					out.Status, out.Err = tasks.StatusFailed, err.Error()
				} else {
					out.Text, _ = res.ModelOutput().(string)
				}
				mgr.OnRunFinished(context.Background(), req.SessionID, out)
			})
			return nil
		}),
		OnTaskUpdate: func(_ context.Context, t *tasks.Task) {
			fmt.Printf("  • task %q → %s\n", t.Label, t.Status)
		},
	})

	tool := sandbox.CodeTool(sb, sandbox.CodeToolConfig{
		SpillDir: ".agents/exec",
		// The hand-off: park the command, spawn the task, answer with its id.
		Background: func(ctx context.Context, tc *agents.ToolContext, cmd *sandbox.Command) (agents.ToolResult, error) {
			mu.Lock()
			pending[cmd.CallID] = cmd
			mu.Unlock()
			state, _ := json.Marshal(commandState{CallID: cmd.CallID})
			info, err := mgr.Spawn(ctx, tasks.SpawnRequest{
				ParentSessionID: tasks.DefaultSessionID(tc.RunContext),
				Input:           cmd.Line,
				Label:           cmd.Line,
				ToolCallID:      cmd.CallID,
				Kind:            kindCommand,
				State:           state,
			})
			if err != nil {
				mu.Lock()
				delete(pending, cmd.CallID)
				mu.Unlock()
				return agents.ToolResult{}, fmt.Errorf("start in the background: %w", err)
			}
			return agents.TextResult("Started in the background as task " + info.TaskID +
				". Finish your reply and end your turn; the result arrives in a later turn.").
				WithDetails(map[string]any{"task_id": info.TaskID}), nil
		},
	})

	if _, err := repo.Create(ctx, session.CreateOptions{ID: "parent", Title: "chat"}); err != nil {
		return err
	}
	agent := &agents.Agent{
		Name:  "operator",
		Model: "gpt-4.1-mini",
		Instructions: agents.StaticInstructions(
			"You run shell commands for the person. A command that takes more than a few seconds " +
				"goes in the background; then end your turn and wait to be told the result."),
		Tools: append([]*agents.Tool{tool}, mgr.Tools(nil)...),
	}
	opts := agents.RunOptions{Model: agents.ModelOptions{Provider: provider}, Context: "parent"}

	fmt.Println("turn 1 — the model starts the command…")
	res, err := agents.RunSync(ctx, agent, "Run `sleep 3; echo built; ls -la` in the background and tell me when it is done.", opts)
	if err != nil {
		return err
	}
	fmt.Printf("model: %s\n", res.FinalOutputString())

	// The command ends on its own goroutine; a server's run boundary would
	// deliver the wake-up, here the program does.
	wg.Wait()
	all, err := store.ListByParent(ctx, "parent")
	if err != nil {
		return err
	}
	wake := tasks.DefaultNotifyFormatter(all)
	fmt.Printf("\nturn 2 — the wake-up:\n%s\n", wake)
	res, err = agents.RunSync(ctx, agent, wake, opts)
	if err != nil {
		return err
	}
	fmt.Printf("model: %s\n", res.FinalOutputString())
	return nil
}
