// Frontend mirror of the WebSocket protocol constants in
// internal/protocol/messages.go. Every ws.on(...)/ws.send(...) and every
// RunError.code branch must reference these — never a string literal — so a
// typo is a compile-time unknown property instead of an event that silently
// never fires. Keep in sync with the Go constants when adding an event.

export const EV = {
  // client → server
  auth: 'auth',
  runCreate: 'run.create',
  runCancel: 'run.cancel',
  runSubscribe: 'run.subscribe',
  // Inject input into a live run; the payload's queue field ('steer' |
  // 'next_turn' | 'follow_up') names the semantics: steer changes course
  // inside the run that is going, next_turn rides along with a turn it was
  // taking anyway, follow_up starts the next exchange.
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
  // A partial result from a tool that is still running. NOT its answer —
  // run.tool_result is — so it renders as live output and is replaced.
  runToolProgress: 'run.tool_progress',
  runHandoff: 'run.handoff',
  runOutput: 'run.output',
  runError: 'run.error',
  runInterrupted: 'run.interrupted',
  runCancelled: 'run.cancelled',
  runCompaction: 'run.compaction',
  // Trouble a run went through and SURVIVED: retries, a fallback model, a
  // compaction pass that gave up. None of these reach run.error.
  runDiagnostic: 'run.diagnostic',
  runGap: 'run.gap',
  sessionTitleUpdated: 'session.title_updated',
  // The session's first sandbox-carrying run permanently bound
  // (sandbox_id, work_dir) — published once, by the run that won the bind.
  sessionSandboxBound: 'session.sandbox_bound',
  traceSpan: 'trace.span',

  // terminal (on /ws/terminal; binary frames carry the byte stream)
  terminalOpen: 'terminal.open',
  terminalResize: 'terminal.resize',
  terminalReady: 'terminal.ready',
  terminalError: 'terminal.error',
  terminalExit: 'terminal.exit',
} as const;

// RunError.code values the client branches on for recovery behavior.
//
// Two origins share one flat namespace (see cmd/agents-server/PROTOCOL.md F3):
// TRANSPORT codes mirror internal/protocol/messages.go — failures that happen
// before or outside a run; SDK codes mirror agents.ErrorCode and grow with the
// SDK, so an unrecognized code MUST fall back to generic error rendering rather
// than being treated as impossible.
export const ERR = {
  // Transport — mirrors protocol.Code* in messages.go
  sessionBusy: 'session_busy',
  sessionNotFound: 'session_not_found',
  runNotFound: 'run_not_found',
  approvalFailed: 'approval_failed',
  configError: 'config_error',

  // SDK — mirrors agents.Code* in agents/errors.go
  guardrailTripwire: 'guardrail_tripwire',
  maxTurns: 'max_turns_exceeded',
  modelBehavior: 'model_behavior',
  modelRefusal: 'model_refusal',
  userError: 'user_error',
  toolTimeout: 'tool_timeout',
  toolPanic: 'tool_panic',
  sandboxExec: 'sandbox_exec',
  mcp: 'mcp',
  unknown: 'unknown',
} as const;

// Mirror of protocol.TaskNotificationPrefix: a user-input message the server
// injects when a background task finishes. Rendered as a notification card,
// not a user bubble.
export const TASK_NOTIFICATION_PREFIX = '[task-notification] ';

// parseTaskNotification is THE way the UI recognizes a server-injected task
// notification (a user-role item the model reads verbatim). Every place that
// special-cases user messages (TOC rail, trace labels, bubble rendering) must
// go through it so the notification is exempted consistently.
export interface TaskNotificationItem {
  label: string;
  taskId: string;
  status: string;
  /** The truncated result, or '' when the task reported none. */
  summary: string;
  /** True when the full result is longer than the summary shown here. */
  truncated: boolean;
}

// The wire format is produced by the SDK's tasks.DefaultNotifyFormatter, and
// this mirrors its tasks.ParseNotification. Keep the two in step: a change to
// the wording there is a change here.
//
// The label is Go-quoted (%q), so it may contain escaped quotes and the naive
// `"([^"]+)"` would stop at the first one. The id is opaque — the host mints
// it — so it is not assumed to be hex.
const TASK_LINE = /^Task "((?:[^"\\]|\\.)*)" \(([^)]+)\) (\w+)\.(?: Result: (.*))?$/;
const TRUNCATION = / \[truncated — call task_status\([^)]+\) for the full result\]$/;

export function parseTaskNotification(content: string | undefined | null): null | { text: string; label: string | null; taskId: string | null; items: TaskNotificationItem[] } {
  if (!content || !content.startsWith(TASK_NOTIFICATION_PREFIX)) return null;
  const text = content.slice(TASK_NOTIFICATION_PREFIX.length);
  // One line per finished task — one wake-up carries every task that owes the
  // parent a notification.
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

// MCP-Tasks-aligned task statuses (mirror of the Go protocol.Task* consts).
export type TaskStatus = 'working' | 'input_required' | 'completed' | 'failed' | 'cancelled';

// RunDiagnostic mirrors protocol.RunDiagnostic. `type` is an open vocabulary:
// an unrecognized one is shown generically rather than dropped, which is what
// lets the server report a new kind without a coordinated release.
export interface RunDiagnostic {
  type: string;
  code?: string;
  message?: string;
  details?: Record<string, unknown>;
}

// DIAGNOSTIC_LABELS gives the known kinds a readable name. Anything absent
// falls back to the raw type.
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
