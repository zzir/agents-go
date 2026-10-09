// Frontend mirror of the WebSocket protocol constants in internal/protocol/messages.go.
// Every ws.on/ws.send and RunError.code branch references these, never a string
// literal, so a typo fails to compile — invariant 15. Keep in sync with the Go side.

export const EV = {
  // client → server
  auth: 'auth',
  runCreate: 'run.create',
  runCancel: 'run.cancel',
  runSubscribe: 'run.subscribe',
  // Queue input on a live run; the payload's queue field is an InjectQueue.
  runInject: 'run.inject',
  toolApprove: 'tool.approve',
  toolReject: 'tool.reject',

  // server → client
  authOk: 'auth.ok',
  runStarted: 'run.started',
  runAgentStart: 'run.agent_start',
  runStep: 'run.step',
  runReasoning: 'run.reasoning',
  runMessage: 'run.message',
  runReasoningItem: 'run.reasoning_item',
  runToolCall: 'run.tool_call',
  runToolResult: 'run.tool_result',
  // Live output of a tool still running; run.tool_result replaces it.
  runToolProgress: 'run.tool_progress',
  runHandoff: 'run.handoff',
  runOutput: 'run.output',
  runError: 'run.error',
  runInterrupted: 'run.interrupted',
  runCancelled: 'run.cancelled',
  runCompaction: 'run.compaction',
  // The run read an input queued on it, numbered within the run so a replay is
  // recognized.
  runInjected: 'run.injected',
  // Trouble a run survived (retries, a fallback model, a failed compaction);
  // never run.error.
  runDiagnostic: 'run.diagnostic',
  runGap: 'run.gap',
  sessionTitleUpdated: 'session.title_updated',
  // The session's project binding, published once by the run that won the bind.
  sessionProjectBound: 'session.project_bound',
  // A session's server-derived status (GET /sessions carries the same);
  // broadcast, never replayed.
  sessionStatus: 'session.status',
  // A session's background task changed state; rides the task run's stream and
  // carries the row as the tasks list returns it, so the client merges rather
  // than refetches.
  taskUpdated: 'task.updated',
  traceSpan: 'trace.span',

  // terminal (on /ws/terminal; binary frames carry the byte stream)
  terminalOpen: 'terminal.open',
  terminalResize: 'terminal.resize',
  terminalReady: 'terminal.ready',
  terminalError: 'terminal.error',
  terminalExit: 'terminal.exit',
} as const;

// RunError.code values the client branches on: transport codes mirror protocol.Code*,
// SDK codes agents.ErrorCode; an unrecognized code renders as a generic error.
export const ERR = {
  // Transport — mirrors protocol.Code* in messages.go
  sessionBusy: 'session_busy',
  sessionNotFound: 'session_not_found',
  runNotFound: 'run_not_found',
  approvalFailed: 'approval_failed',
  configError: 'config_error',
  persistError: 'persist_error',
  streamError: 'stream_error',
  resumeError: 'resume_error',
  providerError: 'provider_error',
  contextOverflow: 'context_overflow',

  // SDK — mirrors agents.Code* in agents/errors.go
  guardrailTripwire: 'guardrail_tripwire',
  maxTurns: 'max_turns_exceeded',
  modelBehavior: 'model_behavior',
  modelRefusal: 'model_refusal',
  userError: 'user_error',
  toolTimeout: 'tool_timeout',
  toolPanic: 'tool_panic',
  toolLoop: 'tool_loop',
  sandboxExec: 'sandbox_exec',
  mcp: 'mcp',
  unknown: 'unknown',
} as const;

// Mirror of protocol.TaskNotificationPrefix: the user-input message the
// server injects when a background task finishes (invariant 21).
export const TASK_NOTIFICATION_PREFIX = '[task-notification] ';

export interface TaskNotificationItem {
  label: string;
  taskId: string;
  status: string;
  /** The truncated result, or '' when the task reported none. */
  summary: string;
  /** True when the full result is longer than the summary shown here. */
  truncated: boolean;
}

// Mirrors the SDK's tasks.ParseNotification — a wording change there is a change
// here. The label is Go-quoted (%q); the id is opaque, never assumed hex.
const TASK_LINE = /^Task "((?:[^"\\]|\\.)*)" \(([^)]+)\) (\w+)\.(?: Result: (.*))?$/;
const TRUNCATION = / \[truncated — call task_status\([^)]+\) for the full result\]$/;

// parseTaskNotification is THE way the UI recognizes a server-injected task
// notification.
export function parseTaskNotification(content: string | undefined | null): null | { text: string; label: string | null; taskId: string | null; items: TaskNotificationItem[] } {
  if (!content || !content.startsWith(TASK_NOTIFICATION_PREFIX)) return null;
  const text = content.slice(TASK_NOTIFICATION_PREFIX.length);
  // One line per finished task: a wake-up carries every task owing a notification.
  const items: TaskNotificationItem[] = [];
  for (const line of text.split('\n')) {
    const m = line.trim().match(TASK_LINE);
    if (!m) continue;
    let summary = m[4] ?? '';
    const truncated = TRUNCATION.test(summary);
    if (truncated) summary = summary.replace(TRUNCATION, '').trim();
    items.push({
      label: m[1].replace(/\\(.)/g, '$1'),
      taskId: m[2],
      status: m[3],
      summary,
      truncated,
    });
  }
  const first = items[0];
  return { text, label: first ? first.label : null, taskId: first ? first.taskId : null, items };
}

// The queues run.inject and POST /runs/:id/inject take (mirror of
// protocol.InjectQueue*).
export type InjectQueue = 'steer' | 'next_turn' | 'follow_up';

// A session's derived status (mirror of protocol.Session*), highest priority first.
export type SessionStatus = 'requires_action' | 'running' | 'failed' | 'idle';

/** The session.status payload, and the same fields on a GET /sessions row. */
export interface SessionStatusEvent {
  session_id: string;
  status: SessionStatus;
  live_run_id?: string;
  pending_count: number;
  oldest_pending_at?: string;
}

// MCP-Tasks-aligned task statuses (mirror of the Go protocol.Task* consts).
export type TaskStatus = 'working' | 'input_required' | 'completed' | 'failed' | 'cancelled';

// TaskKindWorkflow is the task kind of a workflow execution (store.TaskKindWorkflow).
export const TASK_KIND_WORKFLOW = 'workflow';
// The "tool" a workflow step waiting for a go-ahead pauses on
// (store.StepApprovalToolName): approving starts the step, rejecting cancels
// the execution.
export const STEP_APPROVAL_TOOL = 'start_step';

// The decisions Approve all leaves to a person one by one (mirror of the
// bridge's perCallApprovals).
export const PER_CALL_APPROVALS = new Set(['submit_plan', 'save_workflow', 'memory_write', 'memory_append']);

// WorkflowState is a workflow task's `state`: the definition snapshot and where
// the sequence stands (mirror of store.WorkflowState).
export interface WorkflowState {
  workflow_id?: string;
  steps: { id: string; name?: string; agent_config_id?: string; gate?: { pass?: string; fail?: string } | null; pause_before?: boolean }[];
  input?: string;
  step_id: string;
  // Every launched (step, run); outcome (completed | failed | pass | fail) and
  // ended_at are set once the sequence moved on from that run.
  step_runs?: { step_id: string; run_id: string; outcome?: string; started_at?: string; ended_at?: string; retry?: boolean }[];
  pending_input?: string;
  // The definition's budget, snapshotted; absent when it bounds nothing.
  budget?: { max_steps?: number; max_tokens?: number; max_minutes?: number };
  // The bound that ended the execution for good ('budget' | 'ceiling'); no
  // retry is offered.
  stopped?: string;
}

// TaskRow is a background task as GET /sessions/{id}/tasks returns it and as
// task.updated carries it (mirror of store.Task / protocol.TaskUpdated).
export interface TaskRow {
  task_id: string;
  parent_session_id?: string;
  parent_run_id?: string;
  tool_call_id?: string;
  child_session_id?: string;
  kind?: string;
  label?: string;
  // The agent the task runs as — who a decision it waits on is requested by.
  agent_config_id?: string;
  status?: string;
  attempt?: number;
  max_attempts?: number;
  summary?: string;
  state?: WorkflowState;
  dismissed?: boolean;
  // task.updated only: the decision an input_required task waits on, for a pause
  // with no run event to learn it from (a step waiting to start).
  pending_call_id?: string;
  pending_tool_name?: string;
  created_at?: string;
  updated_at?: string;
}

// RunDiagnostic mirrors protocol.RunDiagnostic. `type` is an open vocabulary: an
// unrecognized one is shown generically, never dropped.
export interface RunDiagnostic {
  type: string;
  code?: string;
  message?: string;
  details?: Record<string, unknown>;
}

// DIAGNOSTIC_LABELS names the known kinds; anything absent falls back to the raw type.
export const DIAGNOSTIC_LABELS: Record<string, string> = {
  model_retry: 'retried',
  model_fallback: 'fallback model',
  stream_error: 'stream interrupted',
  tool_panic: 'tool crashed',
  tool_timeout: 'tool timed out',
  compaction_failed: 'compaction failed',
  response_truncated: 'response truncated',
  context_overflow: 'context overflowed',
};
