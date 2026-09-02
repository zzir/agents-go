import { TASK_KIND_WORKFLOW, type TaskStatus, type WorkflowState } from '@/lib/protocol';
import { taskRetryable, type TaskState } from '@/lib/useAgentSocket';

// BackgroundItem is one piece of background work as the panel shows it. A
// spawned task and a workflow execution are the same thing to a reader — work
// running in a session they are not in, which will report back — and to the
// server too: both are tasks, and a workflow is one whose state carries a step
// sequence. `kind` survives only for what the strip renders differently.
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
  // Hidden from the chat strip, still listed in the panel; a retry clears it.
  dismissed?: boolean;
  // Workflow only: the definition snapshot and the launch log the detail lens
  // lists step by step.
  state?: WorkflowState;
}

// stepProgress reads where a workflow stands from its state: the current
// step's ordinal and name, the COMPLETED fraction (the current step is in
// flight, not done), and — once the sequence's own launches outnumber its
// steps, i.e. an edge led back to a step — how many runs it has taken, so a
// loop shows as one at a glance. A person's retry runs a step again too, but
// is not the sequence looping, so it does not count.
export function stepProgress(state?: WorkflowState): { activity?: string; progress?: number } {
  const steps = state?.steps || [];
  const idx = state ? steps.findIndex(s => s.id === state.step_id) : -1;
  if (idx < 0 || steps.length === 0) return {};
  const step = steps[idx];
  const runs = (state?.step_runs || []).filter(r => !r.retry).length;
  return {
    activity: `step ${idx + 1}/${steps.length}${step?.name ? ' · ' + step.name : ''}${runs > steps.length ? ` · run ${runs}` : ''}`,
    progress: idx / steps.length,
  };
}

// hasTaskInStatus reports whether any of a session's background tasks sits in
// the given status. The sidebar reads it twice — 'working' feeds the running
// (orange) marker, 'input_required' the awaiting (red) one — so a session whose
// only live work is a background workflow still shows the colour its state calls
// for, even when it is not the open conversation.
export function hasTaskInStatus(tasks: Record<string, TaskState> | undefined, status: TaskStatus): boolean {
  for (const t of Object.values(tasks || {})) {
    if (t.status === status) return true;
  }
  return false;
}

export function taskItem(t: TaskState): BackgroundItem {
  const workflow = t.kind === TASK_KIND_WORKFLOW;
  const live = t.status === 'working' || t.status === 'input_required';
  return {
    kind: workflow ? 'workflow' : 'task',
    id: t.taskId,
    label: t.label || t.taskId.slice(0, 8),
    status: t.status,
    childSessionId: t.childSessionId,
    createdAt: t.createdAt,
    updatedAt: t.updatedAt,
    ...(workflow
      ? (live ? stepProgress(t.state) : {})
      : { activity: t.status === 'working' ? t.lastTool : undefined }),
    error: t.status === 'failed' ? t.summary : undefined,
    attempt: t.attempt,
    pendingCallId: t.pendingCallId,
    pendingToolName: t.pendingToolName,
    retryable: taskRetryable(t),
    dismissed: t.dismissed,
    state: workflow ? t.state : undefined,
  };
}

// fmtDuration renders a millisecond span as a compact duration (12s, 4m32s,
// 1h03m).
export function fmtDuration(ms: number): string {
  if (!isFinite(ms) || ms < 0) return '';
  const s = Math.floor(ms / 1000);
  if (s < 60) return s + 's';
  const m = Math.floor(s / 60);
  if (m < 60) return m + 'm' + String(s % 60).padStart(2, '0') + 's';
  return Math.floor(m / 60) + 'h' + String(m % 60).padStart(2, '0') + 'm';
}

// itemDuration: live work ticks against now; finished work is fixed at its
// finish time (updatedAt).
export function itemDuration(it: BackgroundItem, now: number): string {
  if (!it.createdAt) return '';
  const live = it.status === 'working' || it.status === 'input_required';
  const end = live ? now : (it.updatedAt || 0);
  return end > it.createdAt ? fmtDuration(end - it.createdAt) : '';
}

// backgroundItems is the panel's whole input: the session's tasks, live over
// the socket, as one list.
export function backgroundItems(tasks: Record<string, TaskState> | undefined): BackgroundItem[] {
  return Object.values(tasks || {}).map(taskItem);
}

// StepRow is one launched step of a workflow, as the detail lens lists them:
// the log entry joined with what that run's trace says it cost.
export interface StepRow {
  index: number;
  stepId: string;
  name: string;
  runId: string;
  // How the run ended (completed | failed | cancelled …), or 'running' for the
  // current one; a gate's pass/fail is the verdict, and its run completed.
  outcome: string;
  verdict?: 'pass' | 'fail';
  // A run a person's retry launched — the same step again, by hand.
  retry?: boolean;
  tokens?: { input: number; output: number };
  durationMs?: number;
}

// TraceSpanLike is the slice of a trace event stepRows reads (structurally the
// Inspector's TraceEventData), so this module needs nothing from the panel.
export interface TraceSpanLike {
  kind?: string;
  type?: string;
  data?: Record<string, unknown> | null;
  started_at?: string;
  ended_at?: string;
}

// stepRows joins a workflow's launch log with the child session's trace runs
// (keyed by run id, as the Inspector holds them). Cost and duration come from
// the run's spans — generation spans carry the tokens, the earliest start and
// latest end bound the time — and are absent for a run whose spans have not
// been loaded (or that never left any).
export function stepRows(state: WorkflowState | undefined, status: TaskStatus, traceRuns?: Record<string, TraceSpanLike[]>): StepRow[] {
  const steps = state?.steps || [];
  const runs = state?.step_runs || [];
  return runs.map((sr, i) => {
    const idx = steps.findIndex(s => s.id === sr.step_id);
    const spans = (traceRuns?.[sr.run_id] || []).filter(ev => ev.kind === 'span');
    let inp = 0, out = 0, t0 = Infinity, t1 = -Infinity;
    for (const ev of spans) {
      if (ev.type === 'generation' && ev.data) {
        inp += Number(ev.data.input_tokens) || 0;
        out += Number(ev.data.output_tokens) || 0;
      }
      if (ev.started_at) {
        const a = new Date(ev.started_at).getTime();
        const b = ev.ended_at ? new Date(ev.ended_at).getTime() : a;
        if (a < t0) t0 = a;
        if (b > t1) t1 = b;
      }
    }
    const last = i === runs.length - 1;
    // The log's own stamps stand in for a run whose spans are not loaded.
    const logged = sr.started_at && sr.ended_at ? new Date(sr.ended_at).getTime() - new Date(sr.started_at).getTime() : NaN;
    // A run still open shows the task's live status; an ending is recorded
    // in the log itself (the task's status stands in only for rows written
    // before that was so).
    const loggedOutcome = sr.outcome || (last ? (status === 'working' || status === 'input_required' ? 'running' : status) : '');
    const verdict = loggedOutcome === 'pass' || loggedOutcome === 'fail' ? loggedOutcome : undefined;
    return {
      index: idx >= 0 ? idx + 1 : i + 1,
      stepId: sr.step_id,
      name: (idx >= 0 && steps[idx].name) || sr.step_id,
      runId: sr.run_id,
      outcome: verdict ? 'completed' : loggedOutcome,
      verdict,
      retry: sr.retry || undefined,
      tokens: inp > 0 || out > 0 ? { input: inp, output: out } : undefined,
      durationMs: isFinite(t0) ? Math.max(t1 - t0, 0) : (isFinite(logged) ? Math.max(logged, 0) : undefined),
    };
  });
}
