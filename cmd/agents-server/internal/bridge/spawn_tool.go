package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/tasks"
	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// The model's whole background surface is the SDK's four verbs — spawn, status,
// retry, stop — and a workflow is not a fifth: it is what spawn_task starts
// when told a workflow's name. One vocabulary, one card, one way to ask after
// it, whatever kind of work it was.

// SpawnToolName is the model's one way to start background work.
const SpawnToolName = "spawn_task"

// spawnArgs is the SDK spawn tool's shape plus the workflow field.
type spawnArgs struct {
	AgentName string `json:"agent_name" jsonschema:"Agent to run a free-form task with; empty uses the current agent. Ignored when workflow is set"`
	Workflow  string `json:"workflow" jsonschema:"Name of a predefined workflow to run instead of a free-form task — a fixed sequence of steps by chosen agents (see the tool description for what is available). Empty for a free-form task"`
	Input     string `json:"input" jsonschema:"The task prompt, or for a workflow its brief: what to do and everything it needs to know. Background work runs in its own session and cannot see this conversation"`
	Label     string `json:"label" jsonschema:"Short human-readable task label shown in the UI (a workflow is labeled by its name)"`
}

// spawnTool is the server's spawn_task: the SDK's, with a workflow field. It
// is built per run because the workflows on offer change without a restart;
// the description lists them only when there are any, so a server without
// workflows offers the plain tool.
func (r *Runner) spawnTool(ctx context.Context) *agents.Tool {
	var offered []store.Workflow
	if r.Deps.Workflows != nil {
		list, err := r.Deps.Workflows.List(ctx)
		if err != nil {
			logging.Ctx(ctx).Warn("spawn_task: listing workflows for the description", "error", err)
		}
		offered = list
	}
	var b strings.Builder
	b.WriteString("Start a background task: another agent works on the input while you continue. " +
		"Returns a task_id immediately. When the task finishes you are notified automatically in a later turn — " +
		"after spawning, finish your reply and END YOUR TURN instead of polling. " +
		"Only reach for task_status when the user explicitly asks for progress (use its wait_seconds rather than a polling loop); task_stop cancels.")
	if len(offered) > 0 {
		b.WriteString("\n\nSet workflow to run one of these predefined workflows instead — a fixed sequence of steps, each by a chosen agent, " +
			"run as one background task in its own session. It shares this session's working directory, so its changes to files are real, " +
			"but it cannot see this conversation — put everything it needs in the input. Available:\n")
		for _, w := range offered {
			fmt.Fprintf(&b, "- %s: %s\n", w.Name, w.Description)
		}
	}

	return agents.NewTool(SpawnToolName, strings.TrimRight(b.String(), "\n"),
		func(ctx context.Context, tc *agents.ToolContext, args spawnArgs) (agents.ToolResult, error) {
			parent, _ := tc.Context.(string)
			if parent == "" {
				return agents.ToolResult{}, fmt.Errorf("spawn_task: no session in the run context")
			}
			if name := strings.TrimSpace(args.Workflow); name != "" {
				return r.spawnWorkflow(ctx, tc, parent, name, strings.TrimSpace(args.Input))
			}
			info, err := r.tasks.Spawn(ctx, tasks.SpawnRequest{
				ParentSessionID: parent,
				AgentName:       args.AgentName,
				Input:           args.Input,
				Label:           args.Label,
				ToolCallID:      tc.ToolCallID,
				ParentRunID:     tasks.ParentRunID(ctx),
			})
			if err != nil {
				return agents.ToolResult{}, err
			}
			// A task that finished before this call returned carries its result
			// in the output, so the model has it — waking later to repeat it
			// would burn a turn.
			r.tasks.ModelHasResult(ctx, info)
			return tasks.ToolResult(info, r.tasks.Progress(info)), nil
		})
}

// spawnWorkflow is the workflow branch of spawn_task: start the named
// sequence as an execution of this conversation, briefed by the model. The
// workflows are read NOW, not from the description's listing: a listing that
// failed at the turn's start must not read as "this server has none", and one
// defined since is on offer.
func (r *Runner) spawnWorkflow(ctx context.Context, tc *agents.ToolContext, parent, name, input string) (agents.ToolResult, error) {
	if r.Deps.Workflows == nil {
		return agents.TextResult("Workflows are not available on this server. Leave workflow empty for a free-form task."), nil
	}
	offered, err := r.Deps.Workflows.List(ctx)
	if err != nil {
		return agents.TextResult(fmt.Sprintf("Could not look up workflows right now: %s. Try again, or leave workflow empty for a free-form task.", err.Error())), nil
	}
	for i := range offered {
		if !strings.EqualFold(offered[i].Name, name) {
			continue
		}
		info, err := r.StartWorkflow(ctx, offered[i].ID, parent, input, tc.ToolCallID)
		if err != nil {
			// The refusal is the model's to read and relay — a budget that is
			// full or an agent that was deleted is something the person can
			// act on.
			return agents.TextResult(fmt.Sprintf("Could not start %q: %s", name, err.Error())), nil
		}
		r.tasks.ModelHasResult(ctx, info)
		// The same card a spawned task gets, so the execution's state follows
		// this call in the transcript; the text leads with what was set going.
		res := tasks.ToolResult(info, r.tasks.Progress(info))
		res.Content = append([]agents.ToolOutputContent{
			agents.ToolOutputText{Text: startedMessage(ctx, r, parent, info, len(offered[i].Steps))},
		}, res.Content...)
		return res, nil
	}
	if len(offered) == 0 {
		return agents.TextResult(fmt.Sprintf("No workflow named %q: this server has none. Leave workflow empty for a free-form task.", name)), nil
	}
	return agents.TextResult(fmt.Sprintf("No workflow named %q. Available: %s", name, workflowNames(offered))), nil
}

// startedMessage tells the model what it just set going — including the one
// thing it cannot see and would otherwise find out about only from a failed
// step: whether this session has a working directory at all.
func startedMessage(ctx context.Context, r *Runner, sessionID string, info *tasks.Info, steps int) string {
	msg := fmt.Sprintf("Started workflow %q (%d steps) as background task %s. You will be told the result; "+
		"do not repeat its work here.", info.Label, steps, info.TaskID)
	if sess, err := r.Deps.Sessions.Get(ctx, sessionID); err == nil && sess.SandboxID == "" {
		msg += " Note: this session has no sandbox bound, so the workflow has no file or command tools."
	}
	return msg
}

func workflowNames(list []store.Workflow) string {
	names := make([]string, 0, len(list))
	for i := range list {
		names = append(names, list[i].Name)
	}
	return strings.Join(names, ", ")
}

// describeTaskState is the manager's DescribeState: where a workflow
// execution stands, in the one line task_status shows beside the status.
func describeTaskState(kind string, state json.RawMessage) string {
	if kind != store.TaskKindWorkflow {
		return ""
	}
	st, err := store.DecodeWorkflowState(state)
	if err != nil {
		return ""
	}
	line := ""
	if idx := st.StepIndex(st.StepID); idx >= 0 {
		line = fmt.Sprintf("step %d/%d", idx+1, len(st.Steps))
		if name := st.Steps[idx].Name; name != "" {
			line += " (" + name + ")"
		}
		// More runs than steps means the sequence came back to a step: said,
		// so a loop reads as one. A person's retry runs a step again too, but
		// is not the sequence looping.
		if runs := st.StepRuns.SequenceRuns(); runs > len(st.Steps) {
			line += fmt.Sprintf(", run %d", runs)
		}
	}
	if st.Stopped != "" {
		if line != "" {
			line += ", "
		}
		switch st.Stopped {
		case store.StoppedByLaps:
			line += "stopped by its loop bound"
		case store.StoppedByCeiling:
			line += "stopped by the step ceiling"
		default:
			line += "stopped by its " + st.Stopped
		}
	}
	return line
}
