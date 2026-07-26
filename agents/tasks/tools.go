package tasks

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zzir/agents-go/agents"
)

// SessionIDFrom is how the tools learn which session they are running in.
//
// The run context's user value is the channel because a tool receives the
// caller's context and nothing else identifying the conversation, and the
// session id decides which parent a task belongs to — getting it from the model
// would let one conversation spawn tasks onto another.
type SessionIDFrom func(rc *agents.RunContext) string

// DefaultSessionID reads a plain string context value, which is what a host
// that has nothing else to carry will use.
func DefaultSessionID(rc *agents.RunContext) string {
	if rc == nil {
		return ""
	}
	s, _ := rc.Context.(string)
	return s
}

type spawnArgs struct {
	AgentName string `json:"agent_name" jsonschema:"Agent to run the task with; empty uses the current agent"`
	Input     string `json:"input" jsonschema:"The task prompt for the agent"`
	Label     string `json:"label" jsonschema:"Short human-readable task label shown in the UI"`
}

type statusArgs struct {
	TaskID      string `json:"task_id" jsonschema:"The id returned by spawn_task"`
	WaitSeconds int    `json:"wait_seconds" jsonschema:"Block up to this many seconds for the task to finish before returning (0 = return immediately). Prefer one wait over repeated polling"`
}

type stopArgs struct {
	TaskID   string `json:"task_id" jsonschema:"The id returned by spawn_task"`
	Graceful bool   `json:"graceful" jsonschema:"Finish the current turn before stopping instead of aborting immediately"`
}

// Tools returns spawn_task, task_status and task_stop.
//
// A task's own run must NOT be given these — that is what bounds recursion —
// so a host attaching them should first ask MetaFor whether the session is a
// task's, and Spawn refuses past the depth limit as the backstop.
//
// sessionID resolves the parent session from the run context; nil uses
// DefaultSessionID.
func (m *Manager) Tools(sessionID SessionIDFrom) []agents.Tool {
	if sessionID == nil {
		sessionID = DefaultSessionID
	}

	spawn := agents.NewFunctionTool("spawn_task",
		"Start a background task: another agent works on the input while you continue. "+
			"Returns a task_id immediately. When the task finishes you are notified automatically in a later turn — "+
			"after spawning, finish your reply and END YOUR TURN instead of polling. "+
			"Only reach for task_status when the user explicitly asks for progress (use its wait_seconds rather than a polling loop); task_stop cancels.",
		func(ctx context.Context, tc *agents.ToolContext, args spawnArgs) (agents.ToolResult, error) {
			parent := sessionID(tc.RunContext)
			if parent == "" {
				return agents.ToolResult{}, fmt.Errorf("spawn_task: no session in the run context")
			}
			info, err := m.Spawn(ctx, SpawnRequest{
				ParentSessionID: parent,
				AgentName:       args.AgentName,
				Input:           args.Input,
				Label:           args.Label,
				// The spawning call id lets the task's later state changes
				// reach the card this call produced, long after the turn ended.
				ToolCallID: tc.ToolCallID,
			})
			if err != nil {
				return agents.ToolResult{}, err
			}
			return taskResult(info), nil
		})

	status := agents.NewFunctionTool("task_status",
		"Check a background task started with spawn_task. Statuses: working, input_required (waiting for a human approval), completed, failed, cancelled. "+
			"For finished tasks the result field carries the FULL final output — the wake-up notification only shows a truncated summary. "+
			"Set wait_seconds for one bounded wait instead of calling this in a loop.",
		func(ctx context.Context, _ *agents.ToolContext, args statusArgs) (agents.ToolResult, error) {
			info, err := m.Status(ctx, args.TaskID, time.Duration(args.WaitSeconds)*time.Second)
			if err != nil {
				return agents.ToolResult{}, err
			}
			return taskResult(info), nil
		})

	stop := agents.NewFunctionTool("task_stop",
		"Stop a background task started with spawn_task.",
		func(ctx context.Context, _ *agents.ToolContext, args stopArgs) (agents.ToolResult, error) {
			info, err := m.Stop(ctx, args.TaskID, args.Graceful)
			if err != nil {
				// A stop of something already finished is news, not a failure:
				// the model should hear the terminal state rather than an error
				// it might retry.
				var final ErrAlreadyFinal
				if info != nil && asAlreadyFinal(err, &final) {
					r := taskResult(info)
					r.IsError = true
					return r, nil
				}
				return agents.ToolResult{}, err
			}
			return taskResult(info), nil
		})

	return []agents.Tool{spawn, status, stop}
}

// taskResult splits what the model reads from what a UI renders: Content is the
// task's state in words, Details is the card's data. A UI parsing the model's
// text back into fields is how the two drift apart.
func taskResult(info *Info) agents.ToolResult {
	return agents.TextResult(describe(info)).
		WithDisplay("task").
		WithDetails(map[string]any{
			"task_id":     info.TaskID,
			"task_label":  info.Label,
			"task_status": string(info.Status),
			"task_agent":  info.Agent,
			"task_result": info.Result,
		})
}

func describe(info *Info) string {
	out := fmt.Sprintf("task_id: %s\nstatus: %s", info.TaskID, info.Status)
	if info.Label != "" {
		out += "\nlabel: " + info.Label
	}
	if info.Agent != "" {
		out += "\nagent: " + info.Agent
	}
	// The full result on a finished task, not the summary: this is the call
	// that exists to fetch it.
	if info.Result != "" {
		out += "\nresult: " + info.Result
	} else if info.Summary != "" {
		out += "\nresult: " + info.Summary
	}
	return out
}

func asAlreadyFinal(err error, target *ErrAlreadyFinal) bool {
	return errors.As(err, target)
}
