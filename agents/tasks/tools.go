package tasks

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zzir/agents-go/agents"
)

// SessionIDFrom is how the tools learn which session they are running in. The
// session id decides which parent a task belongs to, so it comes from the run
// context, never the model — which would let one conversation spawn tasks onto
// another.
type SessionIDFrom func(rc *agents.RunContext) string

// parentRunKey carries the host's identifier for the currently executing run
// (see WithParentRunID).
type parentRunKey struct{}

// WithParentRunID tags ctx with the host's identifier for the executing run.
// spawn_task stamps it onto Task.ParentRunID, which lets a host UI tie the task
// back to the spawning run's trace. Display-only; a host without run
// identifiers can skip it.
func WithParentRunID(ctx context.Context, runID string) context.Context {
	return context.WithValue(ctx, parentRunKey{}, runID)
}

// ParentRunID reads back what WithParentRunID put on ctx: the run a tool call
// is executing inside, empty when the host did not record one.
func ParentRunID(ctx context.Context) string {
	id, _ := ctx.Value(parentRunKey{}).(string)
	return id
}

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
	TaskID      string `json:"task_id" jsonschema:"The id returned by spawn_task; empty lists every task of this conversation instead (newest first, summaries only)"`
	WaitSeconds int    `json:"wait_seconds" jsonschema:"Block up to this many seconds for the task to finish before returning (0 = return immediately). Prefer one wait over repeated polling"`
}

type stopArgs struct {
	TaskID   string `json:"task_id" jsonschema:"The id returned by spawn_task"`
	Graceful bool   `json:"graceful" jsonschema:"Finish the current turn before stopping instead of aborting immediately"`
}

type retryArgs struct {
	TaskID string `json:"task_id" jsonschema:"The id of the failed task to resume"`
}

// Tools returns spawn_task, task_status, task_retry and task_stop — SpawnTool
// followed by TaskTools. A host with more kinds of background work than a
// plain task (a job it starts by name) provides its own spawn tool from the
// public parts (Spawn, ModelHasResult, ToolResult) and attaches TaskTools
// beside it, so the model still sees ONE vocabulary: start, look, retry, stop.
//
// A task's own run must NOT be given these — that is what bounds recursion —
// so a host attaching them should first ask MetaFor whether the session is a
// task's, and Spawn refuses past the depth limit as the backstop.
//
// sessionID resolves the parent session from the run context; nil uses
// DefaultSessionID.
func (m *Manager) Tools(sessionID SessionIDFrom) []*agents.Tool {
	return append([]*agents.Tool{m.SpawnTool(sessionID)}, m.TaskTools(sessionID)...)
}

// SpawnTool is spawn_task alone: start a plain background task with an agent.
func (m *Manager) SpawnTool(sessionID SessionIDFrom) *agents.Tool {
	if sessionID == nil {
		sessionID = DefaultSessionID
	}
	return agents.NewTool("spawn_task",
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
				// The host's id for the executing run (WithParentRunID), so
				// the task ties back to the spawning run's trace.
				ParentRunID: ParentRunID(ctx),
			})
			if err != nil {
				return agents.ToolResult{}, err
			}
			// A task that finished before this call returned carries its result
			// in the tool output below, so the model has it — waking later to
			// repeat it would burn a turn. A no-op for the ordinary
			// still-running case; see ModelHasResult.
			m.ModelHasResult(ctx, info)
			return m.toolResult(info), nil
		})
}

// TaskTools are the three tools that name an existing task: task_status,
// task_retry and task_stop.
func (m *Manager) TaskTools(sessionID SessionIDFrom) []*agents.Tool {
	if sessionID == nil {
		sessionID = DefaultSessionID
	}

	status := agents.NewTool("task_status",
		"Check a background task started with spawn_task. Statuses: working, input_required (waiting for a human approval), completed, failed, cancelled. "+
			"For finished tasks the result field carries the FULL final output — the wake-up notification only shows a truncated summary. "+
			"Set wait_seconds for one bounded wait instead of calling this in a loop. "+
			"With no task_id it lists this conversation's tasks — the way back to an id you no longer have.",
		func(ctx context.Context, tc *agents.ToolContext, args statusArgs) (agents.ToolResult, error) {
			if args.TaskID == "" {
				parent := sessionID(tc.RunContext)
				if parent == "" {
					return agents.ToolResult{}, fmt.Errorf("task_status: no session in the run context")
				}
				infos, err := m.List(ctx, parent)
				if err != nil {
					return agents.ToolResult{}, err
				}
				return agents.TextResult(m.describeList(infos)), nil
			}
			if err := m.ownedBy(ctx, sessionID(tc.RunContext), args.TaskID); err != nil {
				return agents.ToolResult{}, err
			}
			info, err := m.Status(ctx, args.TaskID, time.Duration(args.WaitSeconds)*time.Second)
			if err != nil {
				return agents.ToolResult{}, err
			}
			return m.toolResult(info), nil
		})

	retry := agents.NewTool("task_retry",
		"Resume a FAILED background task from where it stopped: same conversation, same progress, a new attempt. "+
			"Prefer this over spawning a fresh task after a transient failure (a rate limit, a dropped connection) — "+
			"a new task starts from nothing and pays again for everything this one already did. "+
			"Only a failed task can be resumed, and only a limited number of times; when the failure is one a rerun cannot fix, spawn a new task instead.",
		func(ctx context.Context, tc *agents.ToolContext, args retryArgs) (agents.ToolResult, error) {
			if err := m.ownedBy(ctx, sessionID(tc.RunContext), args.TaskID); err != nil {
				return agents.ToolResult{}, err
			}
			info, err := m.Retry(ctx, args.TaskID)
			if err != nil {
				if info == nil {
					// A store failure: the host's problem, with no task state
					// to report on.
					return agents.ToolResult{}, err
				}
				// A refusal, a lost race or a launch that never started: the
				// task's state travels with the error, so the model can decide.
				// Reporting it also settles the wake-up debt, as a success would.
				m.ModelHasResult(ctx, info)
				return m.refusalResult(info, err), nil
			}
			// As with spawn_task: an attempt that finished this fast reports
			// its result here, so nothing is owed.
			m.ModelHasResult(ctx, info)
			return m.toolResult(info), nil
		})

	stop := agents.NewTool("task_stop",
		"Stop a background task started with spawn_task.",
		func(ctx context.Context, tc *agents.ToolContext, args stopArgs) (agents.ToolResult, error) {
			if err := m.ownedBy(ctx, sessionID(tc.RunContext), args.TaskID); err != nil {
				return agents.ToolResult{}, err
			}
			info, err := m.Stop(ctx, args.TaskID, args.Graceful)
			if err != nil {
				// A stop of something already finished is news, not a failure:
				// the model should hear the terminal state rather than an error
				// it might retry.
				if _, ok := errors.AsType[ErrAlreadyFinal](err); info != nil && ok {
					r := m.toolResult(info)
					r.IsError = true
					return r, nil
				}
				return agents.ToolResult{}, err
			}
			return m.toolResult(info), nil
		})

	return []*agents.Tool{status, retry, stop}
}

// ToolResult renders a task for a tool output: what the model reads (Content,
// the state in words) apart from what a UI renders (Details, the card's data).
// progress is the host's one line on where the job stands (Progress), or "".
// Exported for a host tool that starts a task of its own kind and wants the
// same card.
func ToolResult(info *Info, progress string) agents.ToolResult {
	return agents.TextResult(describe(info, progress)).
		WithDisplay("task").
		WithDetails(taskDetails(info))
}

// Progress is the host's line on where a job stands (Config.DescribeState),
// or "" — what ToolResult takes beside the Info.
func (m *Manager) Progress(info *Info) string {
	if m.cfg.DescribeState == nil || info == nil || info.Kind == "" {
		return ""
	}
	return m.cfg.DescribeState(info.Kind, info.State)
}

// toolResult is ToolResult with the host's progress line filled in.
func (m *Manager) toolResult(info *Info) agents.ToolResult {
	return ToolResult(info, m.Progress(info))
}

// refusalResult is a ToolResult whose text leads with why the call was refused —
// the state alone does not explain a refusal the way "already completed" does
// for task_stop.
func (m *Manager) refusalResult(info *Info, err error) agents.ToolResult {
	r := agents.TextResult(err.Error() + "\n" + describe(info, m.Progress(info))).
		WithDisplay("task").
		WithDetails(taskDetails(info))
	r.IsError = true
	return r
}

func taskDetails(info *Info) map[string]any {
	return map[string]any{
		"task_id":      info.TaskID,
		"task_label":   info.Label,
		"task_status":  string(info.Status),
		"task_agent":   info.Agent,
		"task_attempt": info.Attempt,
		"task_result":  info.Result,
	}
}

// describeList is the listing: one line per task, summaries only, the host's
// progress line where it has one. Reading it consumes no wake-up debt —
// nothing here is the full result — so the finish of a task seen in it is
// still delivered. A live task says so, and says what NOT to do: an agent
// with no way to look concluded a task produced nothing and did the work
// over.
func (m *Manager) describeList(infos []*Info) string {
	if len(infos) == 0 {
		return "no tasks in this conversation"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d task(s), newest first (task_status with the id fetches a full result):", len(infos))
	for _, info := range infos {
		fmt.Fprintf(&b, "\n- %s: %s", info.TaskID, info.Status)
		if info.Kind != "" {
			b.WriteString(" · " + info.Kind)
		}
		if info.Label != "" {
			b.WriteString(" · " + info.Label)
		}
		if info.Attempt > 1 {
			fmt.Fprintf(&b, " · attempt %d", info.Attempt)
		}
		if p := m.Progress(info); p != "" {
			b.WriteString(" · " + p)
		}
		if info.Summary != "" {
			b.WriteString(" — " + truncateRunes(info.Summary, m.cfg.SummaryLimit))
		}
		if !info.Status.Terminal() {
			b.WriteString(" (still working — do not redo its work; you will be told when it finishes)")
		}
	}
	return b.String()
}

func describe(info *Info, progress string) string {
	out := fmt.Sprintf("task_id: %s\nstatus: %s", info.TaskID, info.Status)
	// Only past the first attempt — on every task the line is noise the model
	// learns to ignore.
	if info.Attempt > 1 {
		out += fmt.Sprintf("\nattempt: %d", info.Attempt)
	}
	if info.Label != "" {
		out += "\nlabel: " + info.Label
	}
	if info.Kind != "" {
		out += "\nkind: " + info.Kind
	}
	if info.Agent != "" {
		out += "\nagent: " + info.Agent
	}
	if progress != "" {
		out += "\nprogress: " + progress
	}
	// The full result on a finished task, not the summary: this is the call
	// that fetches it.
	if info.Result != "" {
		out += "\nresult: " + info.Result
	} else if info.Summary != "" {
		out += "\nresult: " + info.Summary
	}
	return out
}
