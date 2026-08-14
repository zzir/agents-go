import type { TaskStatus } from '@/lib/protocol';
import { taskRetryable, type TaskState } from '@/lib/useAgentSocket';

// WorkflowRunRow is one execution as /sessions/:id/workflow-runs returns it.
export interface WorkflowRunRow {
  id: string;
  name: string;
  steps?: { id: string; name?: string }[];
  step_id?: string;
  status: string;
  error?: string;
  // The conversation that owns the execution — what the strip filters on, so a
  // single-slot fetch cache can't show another session's rows mid-switch.
  parent_session_id?: string;
  child_session_id?: string;
  // Hidden from the chat strip (the panel still lists it); a retry clears it.
  dismissed?: boolean;
  created_at?: string;
  updated_at?: string;
}

// SessionApproval is one decision this session's background work is waiting on.
// It names the task or the execution it belongs to, never the chat.
export interface SessionApproval {
  task_id?: string;
  workflow_run_id?: string;
  tool_calls?: { tool_call_id: string; tool_name: string }[];
}

// BackgroundItem is one piece of background work as the panel shows it. A
// spawned task and a workflow execution are the same thing to a reader — work
// running in a session they are not in, which will report back — so they share
// one list, one set of states and one detail lens. `kind` survives only where
// they genuinely differ: which endpoint stops or retries it.
export interface BackgroundItem {
  kind: 'task' | 'workflow';
  id: string;
  label: string;
  status: TaskStatus;
  childSessionId?: string;
  createdAt?: number;
  updatedAt?: number;
  // The one-line "what is it doing right now": the running tool for a task,
  // the current step for a workflow.
  activity?: string;
  error?: string;
  // How far a countable sequence has got, 0..1. Absent for a task — it has no
  // measurable middle.
  progress?: number;
  attempt?: number;
  // The approval it is stuck on, answerable from the row.
  pendingCallId?: string;
  pendingToolName?: string;
  retryable: boolean;
  // Workflow only: hidden from the chat strip, still listed in the panel.
  dismissed?: boolean;
}

// An execution's status in the panel's vocabulary. An unmapped one is shown as
// live: a wrong "still running" invites a look, a wrong "completed" hides work
// that is still happening.
const WORKFLOW_STATUS: Record<string, TaskStatus> = {
  running: 'working',
  completed: 'completed',
  failed: 'failed',
  cancelled: 'cancelled',
};

function ms(iso?: string): number | undefined {
  if (!iso) return undefined;
  const t = Date.parse(iso);
  return isNaN(t) ? undefined : t;
}

export function taskItem(t: TaskState): BackgroundItem {
  return {
    kind: 'task',
    id: t.taskId,
    label: t.label || t.taskId.slice(0, 8),
    status: t.status,
    childSessionId: t.childSessionId,
    createdAt: t.createdAt,
    updatedAt: t.updatedAt,
    activity: t.status === 'working' ? t.lastTool : undefined,
    error: t.status === 'failed' ? t.summary : undefined,
    attempt: t.attempt,
    pendingCallId: t.pendingCallId,
    pendingToolName: t.pendingToolName,
    retryable: taskRetryable(t),
  };
}

export function workflowItem(wr: WorkflowRunRow, pending?: SessionApproval): BackgroundItem {
  const steps = wr.steps || [];
  const idx = steps.findIndex(s => s.id === wr.step_id);
  const step = idx >= 0 ? steps[idx] : undefined;
  const waiting = pending?.tool_calls?.[0];
  const status = WORKFLOW_STATUS[wr.status] || 'working';
  return {
    kind: 'workflow',
    id: wr.id,
    label: wr.name || wr.id.slice(0, 8),
    // A step paused on a decision is still `running` on the row — the pause
    // lives in the child run, and only the approval says so.
    status: waiting && status === 'working' ? 'input_required' : status,
    childSessionId: wr.child_session_id,
    createdAt: ms(wr.created_at),
    updatedAt: ms(wr.updated_at),
    activity: idx >= 0 ? `step ${idx + 1}/${steps.length}${step?.name ? ' · ' + step.name : ''}` : undefined,
    error: wr.error,
    // COMPLETED fraction — the current step is in flight, not done (the
    // activity text already says which step is running).
    progress: idx >= 0 && steps.length > 0 ? idx / steps.length : undefined,
    pendingCallId: waiting?.tool_call_id,
    pendingToolName: waiting?.tool_name,
    // Every failed execution can be retried, and it resumes from the step it
    // stopped at rather than starting over.
    retryable: status === 'failed',
    dismissed: wr.dismissed,
  };
}

// mergeBackground is the panel's whole input: the session's tasks (live over
// the socket) and its workflow executions (durable rows), as one list.
export function mergeBackground(
  tasks: Record<string, TaskState> | undefined,
  runs: WorkflowRunRow[] | null | undefined,
  approvals: SessionApproval[] | null | undefined,
): BackgroundItem[] {
  const items = Object.values(tasks || {}).map(taskItem);
  for (const wr of runs || []) {
    items.push(workflowItem(wr, (approvals || []).find(a => a.workflow_run_id === wr.id)));
  }
  return items;
}
