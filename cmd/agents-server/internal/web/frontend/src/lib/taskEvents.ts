import { TASK_KIND_WORKFLOW, type TaskRow, type TaskStatus, type WorkflowState } from '@/lib/protocol';
import {
  ensureLiveTurn, appendMessageItem, appendReasoningItem, finalizeTurn,
  appendErrorPart, appendCancelledPart, appendToolCall, applyToolResult, syncTaskCard, appendToolProgress, appendHandoffPart,
  TERMINAL_TASK_STATUSES,
} from '@/lib/streamReducer';
import { toast } from '@/lib/toast';
import type { SessionState, UpdateSSFn } from '@/lib/useAgentSocket';
import type { TraceEventData as TraceEvent } from '@/features/chat/TracePanel';

// The task side of the run-event stream: a background run's events go to its
// PARENT session's task list and, while the Inspector watches that task, to
// the task view — never to a chat timeline. useAgentSocket asks the router
// first on every event; a handled event is the router's alone.

// TaskState tracks one background task of a chat session — live status from
// run events while the hub run exists, seeded from the durable tasks rows on
// session load.
export interface TaskState {
  taskId: string;
  label: string;
  // The task's kind: undefined/'' a sub-agent task, 'workflow' an execution
  // whose `state` carries the step sequence. A workflow's status is the
  // TASK's, told by task.updated — its step runs end without ending it.
  kind?: string;
  state?: WorkflowState;
  status: TaskStatus;
  // Which run of the task this is: 1 for the original, more after a retry.
  attempt?: number;
  // The ceiling `attempt` is measured against — the server's policy, sent as a
  // PARAMETER so the offer can be derived here (taskRetryable) whenever the
  // status changes.
  maxAttempts?: number;
  childSessionId?: string;
  toolCallId?: string;
  // The run that spawned this task — lets the trace panel nest the wake-up
  // run's card under the run whose spawn_task started the chain.
  parentRunId?: string;
  // Millisecond timestamps for the list's duration label: createdAt from the
  // durable row (or spawn time when seen live), updatedAt refreshed on every
  // task event — for a terminal task it is the finish time.
  createdAt?: number;
  updatedAt?: number;
  lastTool?: string;
  summary?: string;
  // The child run's pending approval, surfaced on the parent's task chip.
  pendingCallId?: string;
  pendingToolName?: string;
  // Hidden from the chat strip (the panel still lists it); a retry clears it.
  dismissed?: boolean;
}

// TaskViewState is the Inspector's live view of ONE task being inspected:
// the child session's transcript (persisted snapshot + live tail assembled
// with the same streamReducer functions as the chat) and its trace events.
// Populated only while the panel is open (watch/unwatch).
export interface TaskViewState {
  taskId: string;
  childSessionId: string;
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  messages: any[];
  streaming: string;
  reasoning: string;
  // Trace events grouped by run — one group per ATTEMPT (a retry starts a new
  // run on the same child session), insertion-ordered oldest first, the same
  // shape the chat's trace drawer keeps.
  traceRuns: Record<string, TraceEvent[]>;
  loaded: boolean;
}

// taskStateFromRow is a durable task row as the socket state holds it — the
// seed before any live event has spoken.
export function taskStateFromRow(row: TaskRow): TaskState {
  return {
    taskId: row.task_id, label: row.label || '', kind: row.kind, state: row.state, toolCallId: row.tool_call_id,
    childSessionId: row.child_session_id, parentRunId: row.parent_run_id,
    status: (row.status || 'working') as TaskStatus, attempt: row.attempt,
    maxAttempts: row.max_attempts, summary: row.summary, dismissed: row.dismissed,
    createdAt: row.created_at ? Date.parse(row.created_at) : undefined,
    updatedAt: row.updated_at ? Date.parse(row.updated_at) : undefined,
  };
}

// taskRetryable answers "would a retry be accepted" from the task state a
// client already tracks: it failed, and it has attempts left against the
// ceiling the server sent. Capacity is NOT part of it — the parent's live-task
// limit is transient, so that refusal arrives as a 409 with its own reason.
export function taskRetryable(t: TaskState): boolean {
  if (t.status !== 'failed' || !t.maxAttempts || (t.attempt || 1) >= t.maxAttempts) return false;
  // An execution its budget or the step ceiling stopped is refused a retry
  // before a run; the state says so.
  return !(t.kind === TASK_KIND_WORKFLOW && t.state?.stopped);
}

// staleTaskRow reports whether a durable row describes an OLDER state than the
// one already on screen: an earlier attempt, or the same attempt walked back
// from a finished status. The same comparison settles two reconnect fetches
// racing each other.
function staleTaskRow(cur: TaskState, status: TaskStatus, attempt?: number): boolean {
  const curAttempt = cur.attempt ?? 0;
  const rowAttempt = attempt ?? 0;
  if (rowAttempt !== curAttempt) return rowAttempt < curAttempt;
  return TERMINAL_TASK_STATUSES.has(cur.status) && !TERMINAL_TASK_STATUSES.has(status);
}

// seedTaskRows folds the durable rows a session load fetched into its state.
// Live events may have registered a task first and win per field; the row
// still owns the durable identity fields (child session, tool call).
export function seedTaskRows(s: SessionState, rows: TaskRow[]): SessionState {
  const tasks = { ...s.tasks };
  for (const row of rows) {
    const created = row.created_at ? Date.parse(row.created_at) : undefined;
    const updated = row.updated_at ? Date.parse(row.updated_at) : undefined;
    const cur = tasks[row.task_id];
    if (cur) {
      tasks[row.task_id] = {
        ...cur,
        label: cur.label || row.label || '',
        kind: cur.kind || row.kind,
        state: cur.state ?? row.state,
        dismissed: cur.dismissed ?? row.dismissed,
        toolCallId: cur.toolCallId || row.tool_call_id,
        childSessionId: cur.childSessionId || row.child_session_id,
        parentRunId: cur.parentRunId || row.parent_run_id,
        attempt: cur.attempt || row.attempt,
        maxAttempts: row.max_attempts ?? cur.maxAttempts,
        summary: cur.summary ?? row.summary,
        createdAt: created || cur.createdAt,
        // For a terminal task the row's updated_at IS the finish time
        // (authoritative); live tasks keep their event-driven value.
        updatedAt: cur.status === 'working' || cur.status === 'input_required'
          ? (cur.updatedAt || updated)
          : (updated || cur.updatedAt),
      };
      continue;
    }
    tasks[row.task_id] = taskStateFromRow(row);
  }
  return { ...s, tasks, tasksLoaded: true };
}

// mergeTaskRows folds durable task rows into a session's state after an
// outage. A snapshot older than what the socket already delivered loses
// (staleTaskRow); the spawn card follows too, not just the chip, since the
// run.started that would have re-armed it is gone from the hub.
export function mergeTaskRows(s: SessionState, rows: TaskRow[]): SessionState {
  const tasks = { ...s.tasks };
  let messages = s.messages;
  for (const row of rows) {
    const cur = tasks[row.task_id];
    const status = (row.status || 'working') as TaskStatus;
    if (cur && staleTaskRow(cur, status, row.attempt)) continue;
    tasks[row.task_id] = {
      ...(cur || { taskId: row.task_id, label: row.label || '', toolCallId: row.tool_call_id }),
      kind: cur?.kind || row.kind,
      state: row.state ?? cur?.state,
      dismissed: row.dismissed,
      childSessionId: cur?.childSessionId || row.child_session_id,
      parentRunId: cur?.parentRunId || row.parent_run_id,
      status, attempt: row.attempt ?? cur?.attempt,
      maxAttempts: row.max_attempts ?? cur?.maxAttempts,
      summary: row.summary ?? cur?.summary,
      createdAt: (row.created_at ? Date.parse(row.created_at) : undefined) || cur?.createdAt,
      updatedAt: (row.updated_at ? Date.parse(row.updated_at) : undefined) || cur?.updatedAt,
    };
    const callId = cur?.toolCallId || row.tool_call_id;
    if (callId) {
      messages = syncTaskCard(messages, callId, {
        id: row.task_id, label: row.label || cur?.label,
        status, summary: row.summary, attempt: row.attempt,
      }) ?? messages;
    }
  }
  return { ...s, tasks, messages };
}

// A durable pending approval that belongs to a background task, as
// GET /sessions/:id/approvals lists it.
export interface TaskPendingApproval {
  task_id?: string;
  tool_calls?: Array<{ tool_call_id: string; tool_name: string }>;
}

// withPendingTaskApprovals puts a paused task's decision on its chip: what
// keeps the Approve button reachable after a reload, when no live
// run.tool_call event will arrive.
export function withPendingTaskApprovals(s: SessionState, pending: TaskPendingApproval[]): SessionState {
  const tasks = { ...s.tasks };
  for (const p of pending) {
    const tc = (p.tool_calls || [])[0];
    if (!p.task_id || !tc) continue;
    const cur = tasks[p.task_id] || { taskId: p.task_id, label: '', status: 'input_required' as const };
    tasks[p.task_id] = { ...cur, status: 'input_required', pendingCallId: tc.tool_call_id, pendingToolName: tc.tool_name };
  }
  return { ...s, tasks };
}

interface TaskRunMeta {
  parentSid: string;
  label: string;
  taskId: string;
  toolCallId?: string;
  kind?: string;
}

export interface TaskRouterDeps {
  updateSS: UpdateSSFn;
  // The child transcript, re-read after a terminal event.
  fetchTimeline: (sid: string) => Promise<{ timeline: SessionState['messages'] }>;
  // Coalesces delta writes to one per frame per key.
  scheduleFrame: (key: string, flush: () => void) => void;
  // The chat's run→session map: the fallback that recognizes a watched
  // child's run whose run.started predates this page.
  sessionOfRun: (runId: string) => string | undefined;
  isDeleted: (sid: string) => boolean;
}

export interface TaskRunStarted {
  session_id?: string; run_id: string; parent_session_id?: string; parent_run_id?: string; task_id?: string;
  kind?: string; tool_call_id?: string; label?: string; attempt?: number; max_attempts?: number;
}

export interface TaskRouter {
  // Every method answers whether the run was a background run and the event
  // was taken; false means the chat path owns it.
  runStarted(p: TaskRunStarted): boolean;
  taskUpdated(p: TaskRow): void;
  step(p: { run_id: string; delta: string }): boolean;
  reasoning(p: { run_id: string; delta: string }): boolean;
  message(p: { run_id: string; text: string; item_id?: string }): boolean;
  reasoningItem(p: { run_id: string; text: string; item_id?: string }): boolean;
  output(p: { run_id: string; final_output?: string }): boolean;
  error(p: { run_id: string; message: string }): boolean;
  cancelled(p: { run_id: string }): boolean;
  toolCall(p: { run_id: string; tool_call_id: string; tool_name: string; arguments: string; needs_approval?: boolean }): boolean;
  toolProgress(p: { run_id: string; call_id: string; delta: string; renderer?: string }): boolean;
  toolResult(p: { run_id: string; tool_call_id: string; output: string; title?: string; summary?: string; renderer?: string; is_error?: boolean }): boolean;
  interrupted(p: { run_id: string }): boolean;
  handoff(p: { run_id: string; to?: string }, handoff: { from: string; to: string; fromId?: string; toId?: string }): boolean;
  traceSpan(runId: string, ev: TraceEvent): boolean;
  watch(sid: string, taskId: string, childSessionId: string): void;
  unwatch(sid: string): void;
  watching(): { sid: string; taskId: string; childSessionId: string } | null;
  // A deleted conversation's runs and watch are dropped so the cascade's
  // late terminal events cannot rebuild it.
  forgetSession(sid: string): void;
}

// createTaskRouter holds the registries for the page's lifetime; `deps` is
// read per call, so the hook hands it the latest callbacks.
export function createTaskRouter(deps: () => TaskRouterDeps): TaskRouter {
  // Background task runs, keyed by child run id. Task identity (task_id) and
  // run (run_id) are separate: events route by run id, state is keyed by task.
  const runs: Record<string, TaskRunMeta> = {};
  // The task the Inspector is watching: its child-run events additionally
  // feed SessionState.taskView (accumulated only while open).
  let watch: { sid: string; taskId: string; childSessionId: string } | null = null;
  let buf = { text: '', reasoning: '' };

  // The watch is keyed by item id; run events carry a run id. The registry
  // maps a run to its item id; the child-session fallback covers a run it
  // never registered (its run.started predates this page).
  const isWatchedRun = (runId: string) => {
    if (!watch) return false;
    return (runs[runId]?.taskId || runId) === watch.taskId || deps().sessionOfRun(runId) === watch.childSessionId;
  };

  // A run whose events belong in a task list or the Inspector, never in a chat
  // timeline: every task run, a workflow step's included.
  const isBackgroundRun = (runId: string) => !!runs[runId] || isWatchedRun(runId);

  // updateTaskView applies fn to the inspected item's view iff runId is one
  // of its runs; the view is keyed by the durable item id, so the guard goes
  // through isWatchedRun rather than comparing raw ids.
  const updateTaskView = (runId: string, fn: (v: TaskViewState) => TaskViewState) => {
    const w = watch;
    if (!w || !isWatchedRun(runId)) return;
    deps().updateSS(w.sid, s => (s.taskView && s.taskView.taskId === w.taskId ? { ...s, taskView: fn(s.taskView) } : s));
  };

  // refetchTaskView re-pulls the child transcript after a terminal event: the
  // snapshot is the durable truth and closes any in-flight merge gap
  // (invariant 17, scoped to the inspected task).
  const refetchTaskView = (runId: string) => {
    const w = watch;
    if (!w || !isWatchedRun(runId)) return;
    // Applied against the watch captured HERE: the terminal handlers drop
    // the run's registry entry as they return.
    deps().fetchTimeline(w.childSessionId).then(({ timeline }) => {
      deps().updateSS(w.sid, s => (s.taskView && s.taskView.taskId === w.taskId
        ? { ...s, taskView: { ...s.taskView, messages: timeline, streaming: '', reasoning: '', loaded: true } }
        : s));
    }).catch(() => undefined).finally(() => {
      // The terminal handler kept the run→task entry alive only for this
      // refetch; a watched task that ended must not leak its routing entry.
      delete runs[runId];
    });
  };

  const updateTask = (runId: string, patch: Partial<TaskState>) => {
    const meta = runs[runId];
    if (!meta) return;
    deps().updateSS(meta.parentSid, s => {
      const cur = s.tasks[meta.taskId] || { taskId: meta.taskId, label: meta.label, status: 'working' as TaskStatus, toolCallId: meta.toolCallId, kind: meta.kind };
      // A workflow's step run ending is NOT the workflow ending: the task
      // moves to its next step, or ends, and task.updated says which. Keep
      // the run-level facts (the approval it dropped), not the verdict.
      if ((meta.kind || cur.kind) === TASK_KIND_WORKFLOW && patch.status && TERMINAL_TASK_STATUSES.has(patch.status)) {
        const { status: _status, summary: _summary, ...runFacts } = patch;
        patch = runFacts;
      }
      let next = { ...s, tasks: { ...s.tasks, [meta.taskId]: { ...cur, updatedAt: Date.now(), ...patch } } };
      // A terminal outcome also folds into the spawn card — the live
      // counterpart of the update entry the server appends (invariant 21).
      if (meta.toolCallId && patch.status) {
        const merged = next.tasks[meta.taskId];
        const msgs = syncTaskCard(next.messages, meta.toolCallId, {
          id: meta.taskId,
          label: merged.label || meta.label,
          status: patch.status,
          summary: patch.summary ?? cur.summary,
          // The attempt travels WITH the outcome, so a replayed run.started
          // for the attempt the card already shows cannot re-arm it.
          attempt: merged.attempt,
        });
        if (msgs) next = { ...next, messages: msgs };
      }
      return next;
    });
  };

  // A terminal event's routing cleanup: the watched run's entry stays for
  // the refetch that still maps runId → taskId when the fetch resolves.
  const endRun = (runId: string) => {
    if (!isWatchedRun(runId)) delete runs[runId];
  };

  return {
    runStarted(p) {
      if (!p.parent_session_id) return false;
      if (deps().isDeleted(p.parent_session_id)) return true;
      const taskId = p.task_id || p.run_id;
      runs[p.run_id] = { parentSid: p.parent_session_id, label: p.label || '', taskId, toolCallId: p.tool_call_id, kind: p.kind };
      deps().updateSS(p.parent_session_id, s => {
        // A retry: the spawn card shows the previous attempt's outcome.
        // Re-arm it (syncTaskCard), or the new outcome is refused as a move
        // backwards.
        const rearmed = p.tool_call_id && p.attempt
          ? syncTaskCard(s.messages, p.tool_call_id, { id: taskId, label: p.label, attempt: p.attempt })
          : null;
        return {
          ...s,
          messages: rearmed || s.messages,
          // Live again, so back on the strip whatever the person hid.
          tasks: { ...s.tasks, [taskId]: { ...s.tasks[taskId], taskId, label: p.label || '', kind: p.kind || s.tasks[taskId]?.kind, status: 'working' as TaskStatus, attempt: p.attempt || s.tasks[taskId]?.attempt, maxAttempts: p.max_attempts || s.tasks[taskId]?.maxAttempts, toolCallId: p.tool_call_id, childSessionId: p.session_id, parentRunId: p.parent_run_id || s.tasks[taskId]?.parentRunId, createdAt: s.tasks[taskId]?.createdAt || Date.now(), updatedAt: Date.now(), summary: undefined, dismissed: false } },
        };
      });
      // A resume segment of the watched task re-announces itself; make sure
      // the view has a live turn to stream into.
      updateTaskView(p.run_id, view => ({ ...view, messages: ensureLiveTurn(view.messages, p.run_id) || view.messages }));
      return true;
    },

    // The task's own state, from the server that owns it. For a sub-agent
    // task it confirms what the run events already said; for a workflow it is
    // the ONLY source of the task-level status and the step it is on. Merged
    // under the same no-move-backwards rule as the durable rows.
    taskUpdated(p) {
      if (!p.parent_session_id || !p.task_id) return;
      const status = (p.status || 'working') as TaskStatus;
      const updated = (p.updated_at ? Date.parse(p.updated_at) : undefined) || Date.now();
      deps().updateSS(p.parent_session_id, s => {
        const cur = s.tasks[p.task_id];
        if (cur && staleTaskRow(cur, status, p.attempt)) return s;
        const paused = status === 'input_required';
        const next: TaskState = {
          ...(cur || { taskId: p.task_id, label: '' }),
          taskId: p.task_id,
          label: p.label || cur?.label || '',
          kind: p.kind || cur?.kind,
          state: p.state ?? cur?.state,
          status,
          attempt: p.attempt ?? cur?.attempt,
          maxAttempts: p.max_attempts ?? cur?.maxAttempts,
          summary: p.summary ?? cur?.summary,
          toolCallId: cur?.toolCallId || p.tool_call_id,
          childSessionId: cur?.childSessionId || p.child_session_id,
          parentRunId: cur?.parentRunId || p.parent_run_id,
          createdAt: cur?.createdAt || updated,
          updatedAt: updated,
          // A task that moved on (a new step, a retry) has no decision pending;
          // only a pause keeps the approval the run events attached.
          pendingCallId: paused ? (p.pending_call_id || cur?.pendingCallId) : undefined,
          pendingToolName: paused ? (p.pending_tool_name || cur?.pendingToolName) : undefined,
          // Live again clears a dismissal — a retry brings the row back; a
          // terminal row carries the flag as the server has it (a dismissal
          // made in another window arrives here), else what this one knows.
          dismissed: TERMINAL_TASK_STATUSES.has(status) ? (p.dismissed ?? cur?.dismissed) : false,
        };
        // The card that spawned it follows the task's state, not its runs'.
        const callId = next.toolCallId;
        const messages = callId
          ? syncTaskCard(s.messages, callId, { id: p.task_id, label: next.label, status, summary: p.summary, attempt: p.attempt }) ?? s.messages
          : s.messages;
        return { ...s, tasks: { ...s.tasks, [p.task_id]: next }, messages };
      });
    },

    step(p) {
      if (!isBackgroundRun(p.run_id)) return false;
      if (isWatchedRun(p.run_id)) {
        buf.text += p.delta;
        deps().scheduleFrame('taskview:step', () => {
          const text = buf.text;
          updateTaskView(p.run_id, v => ({ ...v, streaming: text }));
        });
      }
      return true;
    },

    reasoning(p) {
      if (!isBackgroundRun(p.run_id)) return false;
      if (isWatchedRun(p.run_id)) {
        buf.reasoning += p.delta;
        deps().scheduleFrame('taskview:reasoning', () => {
          const reasoning = buf.reasoning;
          updateTaskView(p.run_id, v => ({ ...v, reasoning }));
        });
      }
      return true;
    },

    message(p) {
      if (!isBackgroundRun(p.run_id)) return false;
      if (p.text && isWatchedRun(p.run_id)) {
        buf.text = '';
        updateTaskView(p.run_id, v => {
          const msgs = appendMessageItem(ensureLiveTurn(v.messages, p.run_id) || v.messages, p.text, !p.item_id);
          return msgs ? { ...v, messages: msgs, streaming: '' } : { ...v, streaming: '' };
        });
      }
      return true;
    },

    reasoningItem(p) {
      if (!isBackgroundRun(p.run_id)) return false;
      if (p.text && isWatchedRun(p.run_id)) {
        buf.reasoning = '';
        updateTaskView(p.run_id, v => {
          const msgs = appendReasoningItem(ensureLiveTurn(v.messages, p.run_id) || v.messages, p.text, !p.item_id);
          return msgs ? { ...v, messages: msgs, reasoning: '' } : { ...v, reasoning: '' };
        });
      }
      return true;
    },

    output(p) {
      if (!isBackgroundRun(p.run_id)) return false;
      updateTask(p.run_id, { status: 'completed', summary: (p.final_output || '').slice(0, 300), pendingCallId: undefined });
      if (isWatchedRun(p.run_id)) {
        const remaining = buf;
        buf = { text: '', reasoning: '' };
        updateTaskView(p.run_id, v => {
          const msgs = finalizeTurn(v.messages, p.final_output || remaining.text, remaining.reasoning);
          return { ...v, messages: msgs || v.messages, streaming: '', reasoning: '' };
        });
        refetchTaskView(p.run_id);
      }
      endRun(p.run_id);
      return true;
    },

    error(p) {
      if (!isBackgroundRun(p.run_id)) return false;
      updateTask(p.run_id, { status: 'failed', summary: p.message?.slice(0, 300), pendingCallId: undefined });
      if (isWatchedRun(p.run_id)) {
        const remaining = buf;
        buf = { text: '', reasoning: '' };
        updateTaskView(p.run_id, v => ({
          ...v,
          messages: appendErrorPart(ensureLiveTurn(v.messages, p.run_id) || v.messages, { type: 'error', content: p.message || 'run failed' }, remaining.reasoning, remaining.text),
          streaming: '', reasoning: '',
        }));
        refetchTaskView(p.run_id);
      }
      endRun(p.run_id);
      return true;
    },

    cancelled(p) {
      if (!isBackgroundRun(p.run_id)) return false;
      updateTask(p.run_id, { status: 'cancelled', pendingCallId: undefined });
      if (isWatchedRun(p.run_id)) {
        const remaining = buf;
        buf = { text: '', reasoning: '' };
        updateTaskView(p.run_id, v => {
          const msgs = appendCancelledPart(v.messages, remaining.reasoning, remaining.text);
          return { ...v, messages: msgs || v.messages, streaming: '', reasoning: '' };
        });
        refetchTaskView(p.run_id);
      }
      endRun(p.run_id);
      return true;
    },

    toolCall(p) {
      if (!isBackgroundRun(p.run_id)) return false;
      updateTask(p.run_id, p.needs_approval
        ? { lastTool: p.tool_name, pendingCallId: p.tool_call_id, pendingToolName: p.tool_name }
        : { lastTool: p.tool_name });
      if (isWatchedRun(p.run_id)) {
        const flushed = buf.text;
        buf.text = '';
        updateTaskView(p.run_id, v => {
          const tc = { tool_call_id: p.tool_call_id, tool_name: p.tool_name, arguments: p.arguments, needs_approval: p.needs_approval || undefined, status: null as string | null, output: null as string | null };
          const msgs = appendToolCall(ensureLiveTurn(v.messages, p.run_id) || v.messages, tc, flushed);
          return msgs ? { ...v, messages: msgs, streaming: '' } : v;
        });
      }
      return true;
    },

    toolProgress(p) {
      if (!isBackgroundRun(p.run_id)) return false;
      if (isWatchedRun(p.run_id)) {
        updateTaskView(p.run_id, v => {
          const msgs = appendToolProgress(v.messages, p.call_id, p.delta, p.renderer);
          return msgs ? { ...v, messages: msgs } : v;
        });
      }
      return true;
    },

    toolResult(p) {
      if (!isBackgroundRun(p.run_id)) return false;
      updateTask(p.run_id, { pendingCallId: undefined, pendingToolName: undefined });
      updateTaskView(p.run_id, v => {
        const msgs = applyToolResult(v.messages, p.tool_call_id, p.output, p);
        return msgs ? { ...v, messages: msgs } : v;
      });
      return true;
    },

    interrupted(p) {
      if (!isBackgroundRun(p.run_id)) return false;
      updateTask(p.run_id, { status: 'input_required' });
      // High-signal, once per pause: background approvals are otherwise easy
      // to miss. A workflow step has no entry here — its name lives on the
      // execution row.
      const meta = runs[p.run_id];
      toast.info(meta ? 'Task "' + (meta.label || p.run_id.slice(0, 8)) + '" needs approval' : 'A workflow step needs approval');
      return true;
    },

    handoff(p, handoff) {
      if (!isBackgroundRun(p.run_id)) return false;
      if (p.to) {
        updateTaskView(p.run_id, v => {
          const msgs = appendHandoffPart(ensureLiveTurn(v.messages, p.run_id) || v.messages, handoff);
          return msgs ? { ...v, messages: msgs } : v;
        });
      }
      return true;
    },

    traceSpan(runId, ev) {
      if (!isBackgroundRun(runId)) return false;
      updateTaskView(runId, v => {
        // Into the span's own run group (a retry's spans open a new group),
        // upserting by span id within it.
        const events = v.traceRuns[runId] || [];
        const idx = ev.span_id ? events.findIndex(e => e.span_id === ev.span_id) : -1;
        const next = idx >= 0 ? [...events.slice(0, idx), ev, ...events.slice(idx + 1)] : [...events, ev];
        return { ...v, traceRuns: { ...v.traceRuns, [runId]: next } };
      });
      return true;
    },

    watch(sid, taskId, childSessionId) {
      watch = { sid, taskId, childSessionId };
      buf = { text: '', reasoning: '' };
    },

    unwatch(sid) {
      watch = null;
      buf = { text: '', reasoning: '' };
      deps().updateSS(sid, s => (s.taskView ? { ...s, taskView: null } : s));
    },

    watching() {
      return watch;
    },

    forgetSession(sid) {
      for (const [runId, meta] of Object.entries(runs)) {
        if (meta.parentSid === sid) delete runs[runId];
      }
      if (watch?.sid === sid) watch = null;
    },
  };
}
