package bridge

import (
	"context"
	"fmt"

	"github.com/zzir/agents-go/agents"
)

// maxConcurrentTasks caps live background tasks per chat session; a spawn
// beyond the cap fails with a model-readable error. Matches the single-digit
// defaults of comparable agent harnesses.
const maxConcurrentTasks = 6

// TaskSpawner is the runner surface the task tools call. It is an interface
// (implemented by *Runner) so agent building doesn't depend on the runner.
type TaskSpawner interface {
	SpawnTask(ctx context.Context, parentSessionID, agentName, input, label string) (*TaskInfo, error)
	TaskStatus(ctx context.Context, taskID string, waitSeconds int) (*TaskInfo, error)
	StopTask(taskID string, graceful bool) (*TaskInfo, error)
}

// TaskInfo is the task tools' model-facing result shape.
type TaskInfo struct {
	TaskID  string `json:"task_id"`
	Label   string `json:"label,omitempty"`
	Agent   string `json:"agent,omitempty"`
	Status  string `json:"status"`
	Summary string `json:"summary,omitempty"`
	// Result carries the task's full final output (task_status only — the
	// wake notification and UI stay on the truncated Summary).
	Result string `json:"result,omitempty"`
}

type spawnTaskArgs struct {
	AgentName string `json:"agent_name" jsonschema:"Agent config name to run the task with; \"default\" (or empty) uses the current agent"`
	Input     string `json:"input" jsonschema:"The task prompt for the agent"`
	Label     string `json:"label" jsonschema:"Short human-readable task label shown in the UI"`
}

type taskStatusArgs struct {
	TaskID string `json:"task_id" jsonschema:"The id returned by spawn_task"`
	// One bounded server-side wait replaces a polling loop: it costs zero
	// model round-trips while the task runs.
	WaitSeconds int `json:"wait_seconds" jsonschema:"Block up to this many seconds for the task to finish before returning (0 = return immediately, max 120). Prefer one wait over repeated polling"`
}

type taskStopArgs struct {
	TaskID   string `json:"task_id" jsonschema:"The id returned by spawn_task"`
	Graceful bool   `json:"graceful" jsonschema:"Finish the current turn before stopping instead of aborting immediately"`
}

// TaskTools returns the background-task tool set exposed to chat agents:
// spawn_task starts a subagent run that outlives this turn and returns its
// task id immediately; task_status polls it; task_stop cancels it. Task runs
// themselves never get these tools (depth cap of one level).
func TaskTools(spawner TaskSpawner) []agents.Tool {
	spawn := agents.NewFunctionTool("spawn_task",
		"Start a background task: another agent works on the input while you continue. "+
			"Returns a task_id immediately. When the task finishes you are notified automatically in a later turn — "+
			"after spawning, finish your reply and END YOUR TURN instead of polling. "+
			"Only reach for task_status when the user explicitly asks for progress (use its wait_seconds rather than a polling loop); task_stop cancels.",
		func(ctx context.Context, tc *agents.ToolContext, args spawnTaskArgs) (*TaskInfo, error) {
			parentSessionID, _ := tc.Context.(string)
			if parentSessionID == "" {
				return nil, fmt.Errorf("spawn_task: no session context")
			}
			ctx = withTaskToolCallID(ctx, tc.ToolCallID)
			return spawner.SpawnTask(ctx, parentSessionID, args.AgentName, args.Input, args.Label)
		})
	status := agents.NewFunctionTool("task_status",
		"Check a background task started with spawn_task. Statuses: working, input_required (waiting for a human approval), completed, failed, cancelled. For finished tasks the result field carries the FULL final output (the wake-up notification only shows a truncated summary). Set wait_seconds for one bounded wait instead of calling this in a loop.",
		func(ctx context.Context, _ *agents.ToolContext, args taskStatusArgs) (*TaskInfo, error) {
			return spawner.TaskStatus(ctx, args.TaskID, args.WaitSeconds)
		})
	stop := agents.NewFunctionTool("task_stop",
		"Stop a background task started with spawn_task.",
		func(_ context.Context, _ *agents.ToolContext, args taskStopArgs) (*TaskInfo, error) {
			return spawner.StopTask(args.TaskID, args.Graceful)
		})
	return []agents.Tool{spawn, status, stop}
}
