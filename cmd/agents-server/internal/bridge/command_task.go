package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/tasks"
	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/sandbox"
)

// A background command is a task of kind command: exec_command hands the
// approved line to backgroundCommand, the launcher runs it on the workspace's
// sandbox with no agent, and its ending wakes the parent like any task's —
// invariant 96, decisions §5.97.

// commandAttempts is a command task's retry ceiling: it cannot resume from
// where it stopped, so it runs once.
const commandAttempts = 1

// maxAttemptsFor is tasks.Config.MaxAttemptsFor.
func maxAttemptsFor(t *tasks.Task) int {
	if t.Kind == store.TaskKindCommand {
		return commandAttempts
	}
	return 0
}

// commandState is a command task's Task.State: the call that parked it.
type commandState struct {
	CallID string `json:"call_id"`
}

// commandRun is one background command this process is running.
type commandRun struct {
	taskID, workspaceID string
	cancel              context.CancelFunc
	// done closes once the outcome is recorded and the sandbox released.
	done chan struct{}
}

// commandRuns holds the background commands of this process: parked by call id
// between the hand-off and the launch, live by run id until the end.
type commandRuns struct {
	mu     sync.Mutex
	parked map[string]*sandbox.Command
	live   map[string]*commandRun
}

func (c *commandRuns) park(cmd *sandbox.Command) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.parked == nil {
		c.parked = map[string]*sandbox.Command{}
	}
	c.parked[cmd.CallID] = cmd
}

// take removes and returns the command parked under callID; nil when none is.
func (c *commandRuns) take(callID string) *sandbox.Command {
	c.mu.Lock()
	defer c.mu.Unlock()
	cmd := c.parked[callID]
	delete(c.parked, callID)
	return cmd
}

func (c *commandRuns) start(runID string, run *commandRun) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.live == nil {
		c.live = map[string]*commandRun{}
	}
	c.live[runID] = run
}

func (c *commandRuns) end(runID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.live, runID)
}

// cancel kills the command running as runID; false when none is.
func (c *commandRuns) cancel(runID string) bool {
	c.mu.Lock()
	run := c.live[runID]
	c.mu.Unlock()
	if run == nil {
		return false
	}
	run.cancel()
	return true
}

func (c *commandRuns) ofWorkspace(workspaceID string) []*commandRun {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*commandRun
	for _, run := range c.live {
		if run.workspaceID == workspaceID {
			out = append(out, run)
		}
	}
	return out
}

// backgroundCommand is AgentDeps.BackgroundCommand: the command is parked under
// its call id and a task of kind command spawned on it; the task's card is the
// exec_command call's (invariant 21).
func (r *Runner) backgroundCommand(ctx context.Context, tc *agents.ToolContext, cmd *sandbox.Command) (agents.ToolResult, error) {
	parent, _ := tc.Context.(string)
	switch {
	case r.tasks == nil:
		return agents.ToolResult{}, errors.New("exec_command: background commands need the task store")
	case parent == "":
		return agents.ToolResult{}, errors.New("exec_command: no session in the run context")
	case cmd.CallID == "":
		return agents.ToolResult{}, errors.New("exec_command: a background call needs a call id")
	}
	state, err := json.Marshal(commandState{CallID: cmd.CallID})
	if err != nil {
		return agents.ToolResult{}, err
	}
	r.commands.park(cmd)
	info, err := r.tasks.Spawn(ctx, tasks.SpawnRequest{
		ParentSessionID: parent,
		Input:           cmd.Line,
		Label:           commandLabel(cmd.Line),
		ParentRunID:     tasks.ParentRunID(ctx),
		ToolCallID:      cmd.CallID,
		Kind:            store.TaskKindCommand,
		State:           state,
	})
	if err != nil {
		r.commands.take(cmd.CallID)
		return agents.ToolResult{}, fmt.Errorf("exec_command: starting in the background: %w", err)
	}
	// A command over before this returned carries its verdict here: no wake-up.
	r.tasks.ModelHasResult(ctx, info)
	res := tasks.ToolResult(info, r.tasks.Progress(info))
	if !info.Status.Terminal() {
		res.Content = append([]agents.ToolOutputContent{agents.ToolOutputText{
			Text: "Running in the background as task " + info.TaskID +
				". Its exit code and output reach you in a later turn: finish your reply and end your turn.\n",
		}}, res.Content...)
	}
	res.Display = "terminal"
	res.Details["command"], res.Details["background"] = cmd.Line, true
	return res, nil
}

// commandLabel is the task's label: the command's first line, at most 80 runes.
func commandLabel(line string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(line), "\n")
	if runes := []rune(first); len(runes) > 80 {
		return string(runes[:79]) + "…"
	}
	return first
}

// launchCommand is the launcher's answer for a command task: the parked command
// runs on the parent's workspace sandbox, on its own goroutine, and reports its
// ending as the run's outcome. A command this process never parked (a retry, a
// restart) is refused.
func (r *Runner) launchCommand(ctx context.Context, req tasks.LaunchRequest) error {
	var st commandState
	if err := json.Unmarshal(req.State, &st); err != nil || st.CallID == "" {
		return fmt.Errorf("command task %s: its state names no call", req.TaskID)
	}
	cmd := r.commands.take(st.CallID)
	if cmd == nil {
		return errors.New("a background command runs once: run it again instead")
	}
	in := store.DecodeInherit(req.Inherit)
	if in.WorkspaceID == "" || r.Deps.Workspaces == nil || r.Deps.SandboxManager == nil {
		return errors.New("a background command needs the session's workspace")
	}
	ws, err := r.Deps.Workspaces.Get(ctx, in.WorkspaceID)
	if err != nil {
		return fmt.Errorf("workspace %s: %w", in.WorkspaceID, err)
	}
	spec, err := workspaceSpec(ctx, r.Deps, ws)
	if err != nil {
		return err
	}
	sb, release, err := r.Deps.SandboxManager.Acquire(spec)
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(r.hub.rootCtx)
	run := &commandRun{taskID: req.TaskID, workspaceID: ws.ID, cancel: cancel, done: make(chan struct{})}
	r.commands.start(req.RunID, run)
	go func() {
		defer close(run.done)
		defer release()
		defer cancel()
		res, err := cmd.Run(runCtx, sb)
		r.commands.end(req.RunID)
		if r.hub.rootCtx.Err() != nil {
			// A shutdown: the row stays working for the restart sweep — decisions §5.57.
			return
		}
		out := tasks.RunOutcome{RunID: req.RunID, Status: tasks.StatusCompleted}
		switch {
		case err != nil && runCtx.Err() != nil:
			out.Status = tasks.StatusCancelled
		case err != nil:
			out.Status, out.Err = tasks.StatusFailed, err.Error()
		default:
			// The verdict is the text either way; a non-zero exit or a
			// timeout is a failed task, so the strip and the wake-up say so.
			out.Text, _ = res.ModelOutput().(string)
			if code, _ := res.Details["exit_code"].(int); code != 0 || res.Details["timed_out"] == true {
				out.Status = tasks.StatusFailed
			}
		}
		r.tasks.OnRunFinished(r.hub.rootCtx, req.SessionID, out)
	}()
	return nil
}

// StopWorkspaceCommands ends every background command running on the workspace
// and waits for their sandbox leases to go, so a sandbox stop that follows
// finds the compute free; bounded by ctx and the teardown wait.
func (r *Runner) StopWorkspaceCommands(ctx context.Context, workspaceID string) {
	if r.tasks == nil {
		return
	}
	runs := r.commands.ofWorkspace(workspaceID)
	for _, run := range runs {
		if _, err := r.tasks.Stop(ctx, run.taskID, false); err != nil {
			if _, final := errors.AsType[tasks.ErrAlreadyFinal](err); !final {
				logging.Ctx(ctx).Warn("stopping a background command before a sandbox stop", "error", err, "task_id", run.taskID)
			}
		}
	}
	deadline := time.After(sessionTeardownWait)
	for _, run := range runs {
		select {
		case <-run.done:
		case <-deadline:
			return
		case <-ctx.Done():
			return
		}
	}
}
