package bridge

import (
	"context"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// withholdsTrust reports whether a segment runs without its session's standing
// command trust — invariant 84. asked is a fresh chat segment's own flag; a
// task's run answers from what started it, on every segment.
func (r *Runner) withholdsTrust(ctx context.Context, task *TaskMeta, fresh, asked bool) bool {
	if task == nil {
		return fresh && asked
	}
	return r.runWithheld(task.ParentRunID) || (task.Kind == store.TaskKindWorkflow && r.workflowFromTrigger(ctx, task.TaskID))
}

// runWithheld reports whether runID was started without standing trust.
func (r *Runner) runWithheld(runID string) bool {
	return runID != "" && r.Deps.SandboxManager != nil && r.Deps.SandboxManager.Trust().RunWithheld(runID)
}

// workflowFromTrigger reports whether the task is a workflow execution a
// trigger started. A row or state that cannot be read counts as one.
func (r *Runner) workflowFromTrigger(ctx context.Context, taskID string) bool {
	if r.Deps.Tasks == nil {
		return false
	}
	row, err := r.Deps.Tasks.Get(ctx, taskID)
	if err != nil {
		return true
	}
	if row.Kind != store.TaskKindWorkflow {
		return false
	}
	st, err := store.DecodeWorkflowState(row.State)
	return err != nil || st.Origin.Kind == store.OriginTrigger
}

// wakeWithheld reports whether a wake-up delivers work a trigger started: any
// one such debt in the batch withholds the turn's trust — invariant 84.
func (r *Runner) wakeWithheld(ctx context.Context, batch []store.Wakeup) bool {
	for i := range batch {
		if r.runWithheld(batch[i].ParentRunID) || r.workflowFromTrigger(ctx, batch[i].SourceID) {
			return true
		}
	}
	return false
}
