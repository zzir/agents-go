import { useCallback, useEffect, useRef } from 'react';
import { WSClient } from '@/lib/ws';
import { EV, ERR, type RunDiagnostic } from '@/lib/protocol';
import type { TaskStatus } from '@/lib/protocol';
import { buildTimeline, type DisplayExtra, type EntryView } from '@/lib/timeline';
import {
  ensureLiveTurn, mergeLiveTail, appendMessageItem, appendReasoningItem, finalizeTurn,
  appendErrorPart, appendCancelledPart, appendToolCall, applyToolResult, applyTaskTerminal, startTaskAttempt, appendToolProgress, appendHandoffPart,
  TERMINAL_TASK_STATUSES,
} from '@/lib/streamReducer';
import { api, clearToken } from '@/lib/api';
import { toast } from '@/lib/toast';

import type { TraceEventData as TraceEvent } from '@/features/chat/TracePanel';

// TaskState tracks one background task of a chat session — live status from
// run events while the hub run exists, seeded from the durable tasks rows on
// session load.
export interface TaskState {
  taskId: string;
  label: string;
  status: TaskStatus;
  // Which run of the task this is: 1 for the original, more after a retry.
  attempt?: number;
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
}

// TaskViewState is the Inspector's live view of ONE task being inspected:
// the child session's transcript (persisted snapshot + live tail assembled
// with the same streamReducer functions as the chat) and its trace events.
// Populated only while the panel is open (watchTask/unwatchTask).
export interface TaskViewState {
  taskId: string;
  childSessionId: string;
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  messages: any[];
  streaming: string;
  reasoning: string;
  traces: TraceEvent[];
  loaded: boolean;
}

export interface SessionState {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  messages: any[];
  streaming: string;
  reasoning: string;
  running: boolean;
  compacting: boolean;
  // Trouble the current run survived — retries, a fallback model, a compaction
  // pass that gave up. None of these fail the run, so without recording them a
  // run that answered after a bad time looks exactly like one that answered
  // first time. Cleared when a new run starts.
  diagnostics: RunDiagnostic[];
  traceRuns: Record<string, TraceEvent[]>;
  liveRunId: string | null;
  liveStartedAt: number | null;
  liveAgentName: string | null;
  loaded: boolean;
  // Backwards pagination over the persisted history. entries are the raw rows
  // fetched so far, kept because a later page has to be REBUILT with the ones
  // already shown — buildTimeline folds turns across rows, so prepending a
  // page to the assembled timeline would split a turn at the page boundary.
  // hasMore is false once a fetch comes back short of the page size.
  entries: EntryView[];
  hasMore: boolean;
  loadingMore: boolean;
  tasks: Record<string, TaskState>;
  // The task currently inspected in the side panel, or null.
  taskView: TaskViewState | null;
}

// HISTORY_PAGE is how many entries a session loads up front, and how many each
// "load earlier" adds. Big enough that an ordinary conversation arrives whole,
// small enough that a months-old session opens promptly.
const HISTORY_PAGE = 200;

// TimelinePage is one fetch: the assembled timeline, the raw entries it came
// from (kept so a later page can be rebuilt WITH them), and whether older
// entries remain.
interface TimelinePage {
  timeline: SessionState['messages'];
  entries: EntryView[];
  hasMore: boolean;
}

type UpdateSSFn = (sid: string, updater: (s: SessionState) => SessionState) => void;

export function defaultSS(): SessionState {
  return {
    messages: [], streaming: '', reasoning: '', running: false, compacting: false, diagnostics: [],
    traceRuns: {}, liveRunId: null, liveStartedAt: null, liveAgentName: null, loaded: false,
    entries: [], hasMore: false, loadingMore: false, tasks: {}, taskView: null,
  };
}

export function useAgentSocket(updateSS: UpdateSSFn) {
  const wsRef = useRef<WSClient | null>(null);
  const runMapRef = useRef<Record<string, string>>({});
  const sessionRunRef = useRef<Record<string, string>>({});
  // Background task runs, keyed by child run id (== task id). Task events are
  // routed into the PARENT session's task list — never a chat timeline — so
  // they must be intercepted before the runMap lookup the chat handlers use.
  const taskRunsRef = useRef<Record<string, { parentSid: string; label: string; taskId: string; toolCallId?: string }>>({});
  // The task the Inspector is watching: its child-run events additionally
  // feed SessionState.taskView (accumulated only while open).
  const taskWatchRef = useRef<{ sid: string; taskId: string; childSessionId: string } | null>(null);
  const taskViewBufRef = useRef({ text: '', reasoning: '' });
  const streamBufsRef = useRef<Record<string, string>>({});
  const reasoningBufsRef = useRef<Record<string, string>>({});
  // Per-run set of completed message/reasoning item ids already folded into the
  // timeline. Hub replays (reconnect) re-deliver those events; deduping by item
  // id — rather than by text — keeps a genuinely repeated identical message from
  // being dropped as if it were a replay.
  const appendedItemsRef = useRef<Record<string, Set<string>>>({});
  const loadedRef = useRef<Set<string>>(new Set());
  // Per-session timeline generation, bumped by forgetLoaded (a branch move).
  // A fetch launched before the bump describes a path the session is no longer
  // on; its late resolution must be dropped, not applied — a pre-branch
  // reloadMessages resolving after the regenerate's reload used to put the
  // replaced answer back on screen.
  const timelineGenRef = useRef<Record<string, number>>({});
  // Sessions with a "load earlier" fetch in flight. A ref rather than the
  // loadingMore flag because the guard must hold across the render the flag
  // needs to land, and React may run the state updater twice.
  const loadingMoreRef = useRef<Set<string>>(new Set());

  // Coalesce high-frequency delta updates (run.step / run.reasoning) to one
  // setState per animation frame per key — buffers accumulate synchronously
  // in refs above, so no data is lost, only renders are batched.
  const rafPendingRef = useRef<Map<string, () => void>>(new Map());
  const rafIdRef = useRef(0);
  const scheduleFrame = useCallback((key: string, flush: () => void) => {
    rafPendingRef.current.set(key, flush);
    if (!rafIdRef.current) {
      rafIdRef.current = requestAnimationFrame(() => {
        rafIdRef.current = 0;
        const fns = Array.from(rafPendingRef.current.values());
        rafPendingRef.current.clear();
        for (const fn of fns) fn();
      });
    }
  }, []);

  // fetchTimeline is the single authority for a session's persisted timeline:
  // messages from the DB merged with the pending tool calls from any durable
  // pending approval. The streaming SDK persists the user prompt (and completed
  // safe-prefix items) up front, so during a pause those already live in
  // `messages`; only the unpaired pending function_call is held back, so the
  // approval row is merged IN (its tool calls attached to the persisted turn),
  // not appended as a fresh user+turn — otherwise the prompt would render twice.
  // Fetching both together and applying the result as one state update removes
  // the messages/approvals race that made the approval card flicker.
  const fetchTimeline = useCallback(async (sid: string, limit = HISTORY_PAGE): Promise<TimelinePage> => {
    type PendingApproval = { run_id: string; user_input?: string; task_id?: string; tool_calls?: Array<{ tool_call_id: string; tool_name: string; arguments: string }> };
    const [msgs, pendingAll] = await Promise.all([
      api.sessions.messages(sid, { limit }) as Promise<EntryView[]>,
      (api.sessions.approvals(sid) as Promise<PendingApproval[]>).catch(() => [] as PendingApproval[]),
    ]);
    // Approvals belonging to background tasks surface on the task chips (and
    // must NOT gate the composer or be merged into the chat timeline — their
    // call ids belong to the task's hidden transcript). Seeding them here is
    // what keeps a paused task's Approve button reachable after a reload,
    // when no live run.tool_call event will ever arrive.
    const taskPending = (pendingAll || []).filter(p => p.task_id);
    if (taskPending.length > 0) {
      updateSS(sid, s => {
        const tasks = { ...s.tasks };
        for (const p of taskPending) {
          const tc = (p.tool_calls || [])[0];
          if (!p.task_id || !tc) continue;
          const cur = tasks[p.task_id] || { taskId: p.task_id, label: '', status: 'input_required' as const };
          tasks[p.task_id] = { ...cur, status: 'input_required', pendingCallId: tc.tool_call_id, pendingToolName: tc.tool_name };
        }
        return { ...s, tasks };
      });
    }
    const entries = msgs || [];
    // A pending approval whose run has off-path entries belongs to an attempt
    // that was branched away (regenerated while paused). Its card must not be
    // rebuilt into the timeline — the branch handler deliberately keeps the
    // row (switching back puts the run's entries on path again, which
    // re-admits it here and lets the pause resume).
    const offPathRuns = new Set(entries.filter(e => e.run_id && e.on_path === false).map(e => e.run_id));
    const pending = (pendingAll || []).filter(p => !p.task_id && !offPathRuns.has(p.run_id));
    // A short page means we reached the beginning; a full one means there may
    // be more, and the next fetch settles it.
    const page = { entries, hasMore: limit > 0 && entries.length >= limit };
    const timeline = buildTimeline(entries) as SessionState['messages'];
    if (!pending || pending.length === 0) return { ...page, timeline };
    const seen = new Set<string>();
    for (const m of timeline) {
      if (m.role !== 'turn') continue;
      for (const part of (m as { parts?: Array<{ type: string; toolCalls?: Array<{ tool_call_id: string }> }> }).parts || []) {
        if (part.type === 'tools') for (const tc of part.toolCalls || []) seen.add(tc.tool_call_id);
      }
    }
    const toolCalls = pending.flatMap(p => (p.tool_calls || []).map(tc => ({
      tool_call_id: tc.tool_call_id, tool_name: tc.tool_name, arguments: tc.arguments,
      output: null as string | null, status: null as string | null, needs_approval: true,
    }))).filter(tc => !seen.has(tc.tool_call_id));
    if (toolCalls.length === 0) return { ...page, timeline };
    const runId = pending[0].run_id;
    const userInput = pending[0].user_input || '';
    const out = [...timeline] as any[];

    // The streaming SDK persists the user prompt (and any completed safe-prefix
    // items) up front, BEFORE the pause — so the timeline may already hold this
    // run's user bubble and turn. Merge the pending tool calls into that turn and
    // skip re-adding the prompt, rather than appending duplicates. Only when
    // nothing for this run is persisted do we reconstruct the paused turn whole.
    let lastTurnIdx = -1;
    for (let i = out.length - 1; i >= 0; i--) {
      if (out[i].role === 'turn') { if (out[i].runId === runId) lastTurnIdx = i; break; }
      if (out[i].role === 'user') break;
    }
    if (lastTurnIdx >= 0) {
      const turn = out[lastTurnIdx];
      const parts = [...(turn.parts || [])];
      const lastPart = parts[parts.length - 1];
      if (lastPart?.type === 'tools') parts[parts.length - 1] = { ...lastPart, toolCalls: [...lastPart.toolCalls, ...toolCalls] };
      else parts.push({ type: 'tools', toolCalls });
      out[lastTurnIdx] = { ...turn, parts };
      return { ...page, timeline: out };
    }
    const hasUser = out.some(m => m.role === 'user' && (m.runId === runId || (userInput && m.content === userInput)));
    if (userInput && !hasUser) out.push({ role: 'user', content: userInput, runId, messageId: Number.MAX_SAFE_INTEGER - 1 });
    out.push({ role: 'turn' as const, parts: [{ type: 'tools' as const, toolCalls }], runId, messageId: Number.MAX_SAFE_INTEGER });
    return { ...page, timeline: out };
  }, []);

  const reloadMessages = useCallback((sid: string) => {
    const gen = timelineGenRef.current[sid] || 0;
    fetchTimeline(sid).then(({ timeline, entries, hasMore }) => {
      // A branch move happened while this fetch was in flight: the response
      // describes the abandoned path — drop it, the move's own reload owns
      // the state.
      if ((timelineGenRef.current[sid] || 0) !== gen) return;
      // Never clobber a running session: mid-resume the paused turn exists
      // NEITHER in messages (saved on completion) nor in approvals (the row is
      // deleted as the resume's claim), so a reload in that window would blank
      // the conversation. Every terminal event sets running=false and reloads,
      // so skipping here loses nothing.
      updateSS(sid, s => s.running ? s : { ...s, messages: timeline, entries, hasMore });
    }).catch(() => {});
  }, [fetchTimeline, updateSS]);

  const loadSession = useCallback((sid: string): Promise<void> => {
    if (!sid || loadedRef.current.has(sid)) return Promise.resolve();
    loadedRef.current.add(sid);
    const gen = timelineGenRef.current[sid] || 0;
    const msgP = fetchTimeline(sid).then(({ timeline, entries, hasMore }) => {
      // Superseded by a later branch move's own reload — drop it (see
      // reloadMessages).
      if ((timelineGenRef.current[sid] || 0) !== gen) return;
      // Live events may have landed while fetching (loaded flipped true, e.g.
      // a broadcast run.started from another browser's run) — merge them onto
      // the persisted snapshot instead of dropping either side. Scoped to the
      // CURRENT live run: a finished or branched-away turn in the tail stays
      // dropped.
      updateSS(sid, s => s.loaded
        ? { ...s, messages: mergeLiveTail(timeline, s.messages, s.liveRunId), entries, hasMore }
        : { ...s, messages: timeline, entries, hasMore, loaded: true });
    }).catch(err => {
      // The fetch failed: roll back the loaded mark so a later retry (or a
      // re-select of this session) re-fetches instead of leaving the history
      // permanently blank, and rethrow so the caller can toast the failure.
      loadedRef.current.delete(sid);
      throw err;
    });
    // Seed the task list from the durable rows; live task-run events (which
    // may already have arrived) win per task id.
    (api.sessions.tasks(sid) as Promise<Array<{ task_id: string; parent_run_id?: string; label?: string; status?: string; attempt?: number; summary?: string; tool_call_id?: string; child_session_id?: string; created_at?: string; updated_at?: string }>>)
      .then(rows => {
        if (!rows || rows.length === 0) return;
        updateSS(sid, s => {
          const tasks = { ...s.tasks };
          for (const row of rows) {
            const created = row.created_at ? Date.parse(row.created_at) : undefined;
            const updated = row.updated_at ? Date.parse(row.updated_at) : undefined;
            const cur = tasks[row.task_id];
            if (cur) {
              // Live events may have registered the task first; the row still
              // owns the durable identity fields (child session, tool call).
              tasks[row.task_id] = {
                ...cur,
                label: cur.label || row.label || '',
                toolCallId: cur.toolCallId || row.tool_call_id,
                childSessionId: cur.childSessionId || row.child_session_id,
                parentRunId: cur.parentRunId || row.parent_run_id,
                attempt: cur.attempt || row.attempt,
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
            tasks[row.task_id] = {
              taskId: row.task_id, label: row.label || '', toolCallId: row.tool_call_id,
              childSessionId: row.child_session_id, parentRunId: row.parent_run_id,
              status: (row.status || 'working') as TaskState['status'], attempt: row.attempt, summary: row.summary,
              createdAt: created, updatedAt: updated,
            };
          }
          return { ...s, tasks };
        });
      }).catch(() => undefined);
    api.sessions.traces(sid).then((events: Array<{ run_id?: string; kind?: string; name?: string; data?: string; detail?: string; error?: string; span_id?: string; parent_id?: string; started_at?: string; ended_at?: string }>) => {
      if (!events || events.length === 0) return;
      const runs: Record<string, TraceEvent[]> = {};
      for (const ev of events) {
        // Spans are the sole trace source; kind=hook rows from older builds
        // are ignored.
        if (ev.kind !== 'span') continue;
        const rid = ev.run_id || 'unknown';
        if (!runs[rid]) runs[rid] = [];
        let parsed: Record<string, unknown> = {};
        if (ev.data) {
          try { parsed = JSON.parse(ev.data); } catch (_e) { /* pre-JSON rows */ }
        }
        let duration = '';
        if (ev.started_at && ev.ended_at) {
          const ms = new Date(ev.ended_at).getTime() - new Date(ev.started_at).getTime();
          duration = ms < 1000 ? ms + 'ms' : (ms / 1000).toFixed(1) + 's';
        }
        runs[rid].push({
          kind: 'span', name: ev.name || '', type: ev.detail || '',
          span_id: ev.span_id, parent_id: ev.parent_id, error: ev.error,
          started_at: ev.started_at, ended_at: ev.ended_at,
          data: Object.keys(parsed).length > 0 ? parsed : null,
          duration,
        });
      }
      // Merge per run id with live data winning: a run.started that landed
      // during this fetch has already seeded (and keeps updating) its own run's
      // entry — the old all-or-nothing guard dropped every persisted run's
      // trace whenever any live run existed.
      updateSS(sid, s => ({ ...s, traceRuns: { ...runs, ...s.traceRuns } }));
    }).catch(() => {});
    return msgP;
  }, [fetchTimeline, updateSS]);

  const deleteSession = useCallback((deletedId: string) => {
    loadedRef.current.delete(deletedId);
  }, []);

  useEffect(() => {
    const ws = new WSClient();
    wsRef.current = ws;

    // The socket kept getting closed before it could authenticate: the token
    // is being rejected. Clear it and drop back to the login screen (mirroring
    // the REST layer's 401 handling) instead of reconnecting forever.
    ws.onAuthFail = () => {
      clearToken();
      window.dispatchEvent(new Event('auth:logout'));
      toast.error('Session expired — please sign in again');
    };

    ws.on(EV.runStarted, (p: { session_id?: string; run_id: string; input?: string; parent_session_id?: string; parent_run_id?: string; task_id?: string; tool_call_id?: string; label?: string; attempt?: number }) => {
      // A background task run: track it under its parent session's task list
      // and keep it out of every chat-timeline path. Task identity (task_id)
      // and run attempt (run_id) are separate: events route by run id through
      // this registry, task state is keyed by task id.
      if (p.parent_session_id) {
        const taskId = p.task_id || p.run_id;
        taskRunsRef.current[p.run_id] = { parentSid: p.parent_session_id, label: p.label || '', taskId, toolCallId: p.tool_call_id };
        updateSS(p.parent_session_id, s => {
          // A retry: the spawn card is showing an outcome that belongs to the
          // attempt before this one. Re-arm it, or the card keeps a stale
          // "failed" badge with a live Retry button, and the new outcome is
          // dropped by applyTaskTerminal's no-move-backwards guard.
          const rearmed = p.tool_call_id && p.attempt
            ? startTaskAttempt(s.messages, p.tool_call_id, p.attempt)
            : null;
          return {
            ...s,
            messages: rearmed || s.messages,
            tasks: { ...s.tasks, [taskId]: { ...s.tasks[taskId], taskId, label: p.label || '', status: 'working' as TaskStatus, attempt: p.attempt || s.tasks[taskId]?.attempt, toolCallId: p.tool_call_id, childSessionId: p.session_id, parentRunId: p.parent_run_id || s.tasks[taskId]?.parentRunId, createdAt: s.tasks[taskId]?.createdAt || Date.now(), updatedAt: Date.now(), summary: undefined } },
          };
        });
        // A resume segment of the watched task re-announces itself; make sure
        // the view has a live turn to stream into.
        updateTaskView(p.run_id, view => ({ ...view, messages: ensureLiveTurn(view.messages, p.run_id) || view.messages }));
        return;
      }
      const sid = p.session_id;
      if (!sid) return;
      runMapRef.current[p.run_id] = sid;
      sessionRunRef.current[sid] = p.run_id;
      streamBufsRef.current[p.run_id] = '';
      reasoningBufsRef.current[p.run_id] = '';
      // Keep any existing set across a replay/resume (same run id) so its dedup
      // memory survives; only seed one for a genuinely new run.
      if (!appendedItemsRef.current[p.run_id]) appendedItemsRef.current[p.run_id] = new Set();
      // Deliberately NOT marking loadedRef here: run events broadcast to every
      // browser, so this may be the first thing a watching browser hears about
      // the session — loadSession must still fetch the history later (its merge
      // keeps the live entries accumulated meanwhile).
      updateSS(sid, s => {
        // Hub replays (reconnect / re-subscribe) re-deliver run.started; the
        // reducer returns null instead of growing a second live turn.
        const appended = ensureLiveTurn(s.messages, p.run_id, p.input);
        return {
          ...s, running: true, compacting: false, diagnostics: [], liveRunId: p.run_id,
          liveStartedAt: appended ? Date.now() : (s.liveStartedAt ?? Date.now()),
          liveAgentName: null, loaded: true,
          messages: appended || s.messages,
          traceRuns: { ...s.traceRuns, [p.run_id]: s.traceRuns[p.run_id] || [] },
        };
      });
    });

    // updateTaskView applies fn to the inspected task's view iff runId is the
    // watched task. Returns true when it was. taskView is keyed by the DURABLE
    // task id, not the run attempt id — so both the outer watch guard and the
    // inner taskView guard must map runId → taskId before comparing (comparing
    // taskView.taskId against the raw runId is always false, which silently
    // dropped every live Inspector update).
    const updateTaskView = (runId: string, fn: (v: TaskViewState) => TaskViewState) => {
      const w = taskWatchRef.current;
      const taskId = taskRunsRef.current[runId]?.taskId ?? runId;
      if (!w || taskId !== w.taskId) return false;
      updateSS(w.sid, s => (s.taskView && s.taskView.taskId === taskId ? { ...s, taskView: fn(s.taskView) } : s));
      return true;
    };

    // refetchTaskView re-pulls the child transcript after a terminal event —
    // the snapshot is the durable truth and closes any in-flight merge gap
    // (the chat path's "terminal events reconcile against the store", scoped
    // to the inspected task).
    const refetchTaskView = (runId: string) => {
      const w = taskWatchRef.current;
      if (!w || (taskRunsRef.current[runId]?.taskId || runId) !== w.taskId) return;
      fetchTimeline(w.childSessionId).then(({ timeline }) => {
        updateTaskView(runId, v => ({ ...v, messages: timeline, streaming: '', reasoning: '', loaded: true }));
      }).catch(() => undefined).finally(() => {
        // This is the terminal refetch (the terminal handler kept the run→task
        // entry alive only for it). Drop it now so a watched task that ended
        // doesn't leak its routing entry for the life of the page.
        delete taskRunsRef.current[runId];
      });
    };

    const updateTask = (runId: string, patch: Partial<TaskState>) => {
      const meta = taskRunsRef.current[runId];
      if (!meta) return false;
      updateSS(meta.parentSid, s => {
        const cur = s.tasks[meta.taskId] || { taskId: meta.taskId, label: meta.label, status: 'working' as TaskStatus, toolCallId: meta.toolCallId };
        let next = { ...s, tasks: { ...s.tasks, [meta.taskId]: { ...cur, updatedAt: Date.now(), ...patch } } };
        // A terminal outcome also folds into the spawn card, so the timeline
        // shows "task completed" and the result summary without a reload —
        // the live counterpart of the call-display UPDATE the server appends
        // for replay (applyTaskTerminal ignores non-terminal statuses).
        if (meta.toolCallId && patch.status) {
          const msgs = applyTaskTerminal(next.messages, meta.toolCallId, {
            id: meta.taskId,
            label: cur.label || meta.label,
            status: patch.status,
            summary: patch.summary ?? cur.summary,
          });
          if (msgs) next = { ...next, messages: msgs };
        }
        return next;
      });
      return true;
    };

    // The Inspector watch is keyed by task id; run events carry a run id —
    // map through the registry before comparing.
    const isWatchedRun = (runId: string) => {
      const w = taskWatchRef.current;
      return !!w && (taskRunsRef.current[runId]?.taskId || runId) === w.taskId;
    };

    ws.on(EV.runStep, (p: { run_id: string; delta: string }) => {
      if (taskRunsRef.current[p.run_id]) {
        if (isWatchedRun(p.run_id)) {
          taskViewBufRef.current.text += p.delta;
          scheduleFrame('taskview:step', () => {
            const buf = taskViewBufRef.current.text;
            updateTaskView(p.run_id, v => ({ ...v, streaming: buf }));
          });
        }
        return;
      }
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      streamBufsRef.current[p.run_id] = (streamBufsRef.current[p.run_id] || '') + p.delta;
      scheduleFrame('step:' + p.run_id, () => {
        const buf = streamBufsRef.current[p.run_id];
        if (buf !== undefined) updateSS(sid, s => ({ ...s, streaming: buf }));
      });
    });

    ws.on(EV.runReasoning, (p: { run_id: string; delta: string }) => {
      if (taskRunsRef.current[p.run_id]) {
        if (isWatchedRun(p.run_id)) {
          taskViewBufRef.current.reasoning += p.delta;
          scheduleFrame('taskview:reasoning', () => {
            const buf = taskViewBufRef.current.reasoning;
            updateTaskView(p.run_id, v => ({ ...v, reasoning: buf }));
          });
        }
        return;
      }
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      reasoningBufsRef.current[p.run_id] = (reasoningBufsRef.current[p.run_id] || '') + p.delta;
      scheduleFrame('reasoning:' + p.run_id, () => {
        const buf = reasoningBufsRef.current[p.run_id];
        if (buf !== undefined) updateSS(sid, s => ({ ...s, reasoning: buf }));
      });
    });

    // One completed assistant message — a turn's full text, interim narration
    // and final answer alike. Authoritative over the run.step deltas that
    // previewed it: the delta buffer is dropped so the tool_call flush (and
    // run.output) cannot append the same text again.
    ws.on(EV.runMessage, (p: { run_id: string; text: string; item_id?: string }) => {
      if (taskRunsRef.current[p.run_id]) {
        if (p.text && isWatchedRun(p.run_id)) {
          taskViewBufRef.current.text = '';
          updateTaskView(p.run_id, v => {
            const msgs = appendMessageItem(ensureLiveTurn(v.messages, p.run_id) || v.messages, p.text, !p.item_id);
            return msgs ? { ...v, messages: msgs, streaming: '' } : { ...v, streaming: '' };
          });
        }
        return;
      }
      const sid = runMapRef.current[p.run_id];
      if (!sid || !p.text) return;
      streamBufsRef.current[p.run_id] = '';
      const seen = appendedItemsRef.current[p.run_id] || (appendedItemsRef.current[p.run_id] = new Set());
      // Hub replays (reconnect / re-subscribe) re-deliver run.message. Dedup by
      // item id when present; only fall back to text equality (which also drops a
      // genuinely repeated identical message) for backends that send no id.
      if (p.item_id && seen.has(p.item_id)) { updateSS(sid, s => ({ ...s, streaming: '' })); return; }
      updateSS(sid, s => {
        const msgs = appendMessageItem(s.messages, p.text, !p.item_id);
        return msgs ? { ...s, messages: msgs, streaming: '' } : { ...s, streaming: '' };
      });
      if (p.item_id) seen.add(p.item_id);
    });

    // One completed reasoning block — a turn's full thinking text,
    // authoritative over the run.reasoning deltas that previewed it. Freezing
    // it as a thinking part (and resetting the delta buffer) scopes the live
    // "Thinking…" preview to the current turn and is the only thinking signal
    // on backends that stream no reasoning deltas.
    ws.on(EV.runReasoningItem, (p: { run_id: string; text: string; item_id?: string }) => {
      if (taskRunsRef.current[p.run_id]) {
        if (p.text && isWatchedRun(p.run_id)) {
          taskViewBufRef.current.reasoning = '';
          updateTaskView(p.run_id, v => {
            const msgs = appendReasoningItem(ensureLiveTurn(v.messages, p.run_id) || v.messages, p.text, !p.item_id);
            return msgs ? { ...v, messages: msgs, reasoning: '' } : { ...v, reasoning: '' };
          });
        }
        return;
      }
      const sid = runMapRef.current[p.run_id];
      if (!sid || !p.text) return;
      reasoningBufsRef.current[p.run_id] = '';
      const seen = appendedItemsRef.current[p.run_id] || (appendedItemsRef.current[p.run_id] = new Set());
      // Hub replays re-deliver run.reasoning_item. Dedup by item id when present;
      // fall back to text equality only when the backend sends none.
      if (p.item_id && seen.has(p.item_id)) { updateSS(sid, s => ({ ...s, reasoning: '' })); return; }
      updateSS(sid, s => {
        const msgs = appendReasoningItem(s.messages, p.text, !p.item_id);
        return msgs ? { ...s, messages: msgs, reasoning: '' } : { ...s, reasoning: '' };
      });
      if (p.item_id) seen.add(p.item_id);
    });

    ws.on(EV.runOutput, (p: { run_id: string; final_output?: string }) => {
      if (taskRunsRef.current[p.run_id]) {
        updateTask(p.run_id, { status: 'completed', summary: (p.final_output || '').slice(0, 300), pendingCallId: undefined });
        if (isWatchedRun(p.run_id)) {
          const remaining = taskViewBufRef.current;
          taskViewBufRef.current = { text: '', reasoning: '' };
          updateTaskView(p.run_id, v => {
            const msgs = finalizeTurn(v.messages, p.final_output || remaining.text, remaining.reasoning);
            return { ...v, messages: msgs || v.messages, streaming: '', reasoning: '' };
          });
          refetchTaskView(p.run_id);
        }
        // Terminal: drop the run→task routing entry so it doesn't accumulate.
        // Keep the watched run's entry — its async refetchTaskView still maps
        // runId → taskId when the fetch resolves.
        if (!isWatchedRun(p.run_id)) delete taskRunsRef.current[p.run_id];
        return;
      }
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      const text = p.final_output || streamBufsRef.current[p.run_id] || '';
      const thinking = reasoningBufsRef.current[p.run_id] || '';
      delete streamBufsRef.current[p.run_id];
      delete reasoningBufsRef.current[p.run_id];
      delete appendedItemsRef.current[p.run_id];
      delete runMapRef.current[p.run_id];
      delete sessionRunRef.current[sid];
      updateSS(sid, s => {
        const msgs = finalizeTurn(s.messages, text, thinking);
        return { ...s, messages: msgs || s.messages, streaming: '', reasoning: '', running: false, compacting: false, liveRunId: null, liveStartedAt: null, liveAgentName: null };
      });
      reloadMessages(sid);
    });

    ws.on(EV.runError, (p: { run_id?: string; session_id?: string; code?: string; message: string; guardrail?: string; stage?: string }) => {
      if (p.run_id && taskRunsRef.current[p.run_id]) {
        updateTask(p.run_id, { status: 'failed', summary: p.message?.slice(0, 300), pendingCallId: undefined });
        if (isWatchedRun(p.run_id)) {
          const remaining = taskViewBufRef.current;
          taskViewBufRef.current = { text: '', reasoning: '' };
          const rid = p.run_id;
          updateTaskView(rid, v => ({
            ...v,
            messages: appendErrorPart(ensureLiveTurn(v.messages, rid) || v.messages, { type: 'error', content: p.message || 'run failed' }, remaining.reasoning, remaining.text),
            streaming: '', reasoning: '',
          }));
          refetchTaskView(rid);
        }
        if (!isWatchedRun(p.run_id)) delete taskRunsRef.current[p.run_id];
        return;
      }
      // The session already has a live run (e.g. double-send from another
      // tab): the run this error names is still executing — a toast, not a
      // terminal error on the live turn. The rejected send left an optimistic
      // user bubble that will never get a run, so roll it back instead of
      // stranding a ghost message. Only this tab's un-sent sends carry a
      // clientMsgId (with no run/message id), so the newest such bubble is
      // exactly the one that was just rejected.
      if (p.code === ERR.sessionBusy) {
        toast.error(p.message || 'Session already has an active run');
        const sid = p.session_id || (p.run_id ? runMapRef.current[p.run_id] : undefined);
        if (sid) {
          updateSS(sid, s => {
            const msgs = s.messages as Array<{ role?: string; clientMsgId?: string; messageId?: number; runId?: string }>;
            for (let i = msgs.length - 1; i >= 0; i--) {
              const m = msgs[i];
              if (m.role === 'user' && m.clientMsgId && m.messageId === undefined && m.runId === undefined) {
                return { ...s, messages: msgs.slice(0, i).concat(msgs.slice(i + 1)) };
              }
            }
            return s;
          });
        }
        return;
      }
      // An approve/reject that failed server-side (session busy, config deleted,
      // stale state): the optimistic 'approved'/'rejected' card status was never
      // rolled back. Rebuild the paused turn from the durable approval row so its
      // pending card and Approve/Reject controls reappear, and surface why.
      if (p.code === ERR.approvalFailed) {
        toast.error(p.message || 'Approval failed');
        const sid = p.session_id || (p.run_id ? runMapRef.current[p.run_id] : undefined);
        if (sid) reloadMessages(sid);
        return;
      }
      // The run we tried to resubscribe expired server-side (finished >15min
      // ago): clear the stale mapping and fall back to persisted history.
      if (p.code === ERR.runNotFound) {
        const staleSid = p.run_id ? runMapRef.current[p.run_id] : undefined;
        if (p.run_id) {
          delete runMapRef.current[p.run_id];
          delete streamBufsRef.current[p.run_id];
          delete reasoningBufsRef.current[p.run_id];
          delete appendedItemsRef.current[p.run_id];
        }
        if (staleSid) {
          delete sessionRunRef.current[staleSid];
          updateSS(staleSid, s => ({ ...s, streaming: '', reasoning: '', running: false, compacting: false, liveRunId: null, liveStartedAt: null, liveAgentName: null }));
          reloadMessages(staleSid);
        }
        return;
      }
      // Failures before run.started (session_not_found, approval_failed)
      // carry session_id instead of a mapped run id.
      const sid = (p.run_id && runMapRef.current[p.run_id]) || p.session_id;
      if (!sid) {
        toast.error(p.message || 'Run failed');
        return;
      }
      const rid = p.run_id || '';
      const remaining = streamBufsRef.current[rid] || '';
      const thinking = reasoningBufsRef.current[rid] || '';
      delete streamBufsRef.current[rid];
      delete reasoningBufsRef.current[rid];
      delete appendedItemsRef.current[rid];
      delete runMapRef.current[rid];
      delete sessionRunRef.current[sid];
      // A guardrail block carries the guardrail name + stage so the turn renders
      // a distinct "blocked" card instead of a generic error.
      const errPart = p.code === ERR.guardrailTripwire
        ? { type: 'error' as const, content: p.message, guardrail: p.guardrail, stage: p.stage }
        : { type: 'error' as const, content: p.message };
      updateSS(sid, s => ({
        ...s, messages: appendErrorPart(s.messages, errPart, thinking, remaining),
        streaming: '', reasoning: '', running: false, compacting: false, liveRunId: null, liveStartedAt: null, liveAgentName: null,
      }));
      // A guardrail block already rendered its typed card (with the retracted
      // answer above it) optimistically. A reload would replace that with the
      // persisted timeline, which — since the SDK never persists a tripped output
      // (Python parity) — drops the answer; the card itself survives via the
      // persisted guardrail/stage. Keep the richer optimistic view for this session.
      if (p.code !== ERR.guardrailTripwire) reloadMessages(sid);
    });

    ws.on(EV.runCancelled, (p: { run_id?: string; code?: string }) => {
      if (p?.run_id && taskRunsRef.current[p.run_id]) {
        updateTask(p.run_id, { status: 'cancelled', pendingCallId: undefined });
        if (isWatchedRun(p.run_id)) {
          const remaining = taskViewBufRef.current;
          taskViewBufRef.current = { text: '', reasoning: '' };
          updateTaskView(p.run_id, v => {
            const msgs = appendCancelledPart(v.messages, remaining.reasoning, remaining.text);
            return { ...v, messages: msgs || v.messages, streaming: '', reasoning: '' };
          });
          refetchTaskView(p.run_id);
        }
        if (!isWatchedRun(p.run_id)) delete taskRunsRef.current[p.run_id];
        return;
      }
      const rid = p?.run_id;
      const sid = rid ? runMapRef.current[rid] : null;
      if (!sid || !rid) return;
      const remaining = streamBufsRef.current[rid] || '';
      const thinking = reasoningBufsRef.current[rid] || '';
      delete streamBufsRef.current[rid];
      delete reasoningBufsRef.current[rid];
      delete appendedItemsRef.current[rid];
      delete runMapRef.current[rid];
      delete sessionRunRef.current[sid];
      // The marker shows immediately, mirroring how run.error appends its card
      // optimistically, instead of waiting on the async reload (which the next
      // run's start can also skip).
      updateSS(sid, s => {
        const msgs = appendCancelledPart(s.messages, thinking, remaining);
        return { ...s, messages: msgs || s.messages, streaming: '', reasoning: '', running: false, compacting: false, liveRunId: null, liveStartedAt: null, liveAgentName: null };
      });
      reloadMessages(sid);
    });

    ws.on(EV.runToolCall, (p: { run_id: string; tool_call_id: string; tool_name: string; arguments: string; needs_approval?: boolean }) => {
      if (taskRunsRef.current[p.run_id]) {
        updateTask(p.run_id, p.needs_approval
          ? { lastTool: p.tool_name, pendingCallId: p.tool_call_id, pendingToolName: p.tool_name }
          : { lastTool: p.tool_name });
        if (isWatchedRun(p.run_id)) {
          const flushed = taskViewBufRef.current.text;
          taskViewBufRef.current.text = '';
          updateTaskView(p.run_id, v => {
            const tc = { tool_call_id: p.tool_call_id, tool_name: p.tool_name, arguments: p.arguments, needs_approval: p.needs_approval || undefined, status: null as string | null, output: null as string | null };
            const msgs = appendToolCall(ensureLiveTurn(v.messages, p.run_id) || v.messages, tc, flushed);
            return msgs ? { ...v, messages: msgs, streaming: '' } : v;
          });
        }
        return;
      }
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      const flushed = streamBufsRef.current[p.run_id] || '';
      streamBufsRef.current[p.run_id] = '';
      // Normalize needs_approval: the wire always carries the bool, but a
      // replayed timeline only ever marks pending calls — carrying an explicit
      // false would make the streamed turn differ from its reload (the
      // isomorphism test pins this).
      const tc = { tool_call_id: p.tool_call_id, tool_name: p.tool_name, arguments: p.arguments, needs_approval: p.needs_approval || undefined, status: null as string | null, output: null as string | null };
      updateSS(sid, s => {
        let msgs = appendToolCall(s.messages, tc, flushed);
        // The spawned task may already be terminal: parent and task runs are
        // delivered on independent subscriptions with no cross-run ordering
        // (a reconnect replays both buffers), so updateTask's terminal fold
        // can run before this card exists and find nothing to patch. The
        // outcome is still in s.tasks — fold it onto the card it was for.
        if (msgs) {
          for (const t of Object.values(s.tasks)) {
            if (t.toolCallId === p.tool_call_id && TERMINAL_TASK_STATUSES.has(t.status)) {
              msgs = applyTaskTerminal(msgs, p.tool_call_id, { id: t.taskId, label: t.label, status: t.status, summary: t.summary }) ?? msgs;
              break;
            }
          }
        }
        return msgs ? { ...s, messages: msgs, streaming: '' } : s;
      });
    });

    // Live output from a tool that is still running. It is not the answer —
    // run.tool_result is — so it accumulates on the card and is replaced when
    // the result lands.
    ws.on(EV.runToolProgress, (p: { run_id: string; call_id: string; delta: string; renderer?: string }) => {
      if (taskRunsRef.current[p.run_id]) {
        if (isWatchedRun(p.run_id)) {
          updateTaskView(p.run_id, v => {
            const msgs = appendToolProgress(v.messages, p.call_id, p.delta, p.renderer);
            return msgs ? { ...v, messages: msgs } : v;
          });
        }
        return;
      }
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      updateSS(sid, s => {
        const msgs = appendToolProgress(s.messages, p.call_id, p.delta, p.renderer);
        return msgs ? { ...s, messages: msgs } : s;
      });
    });

    // Single registration: WSClient.on is overwrite-semantics (one handler per
    // event type), so the task branch must live INSIDE the chat handler.
    ws.on(EV.runToolResult, (p: { run_id: string; tool_call_id: string; output: string; title?: string; summary?: string; renderer?: string; is_error?: boolean; extra?: DisplayExtra }) => {
      if (taskRunsRef.current[p.run_id]) {
        updateTask(p.run_id, { pendingCallId: undefined, pendingToolName: undefined });
        updateTaskView(p.run_id, v => {
          const msgs = applyToolResult(v.messages, p.tool_call_id, p.output, p);
          return msgs ? { ...v, messages: msgs } : v;
        });
        return;
      }
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      updateSS(sid, s => {
        const msgs = applyToolResult(s.messages, p.tool_call_id, p.output, p);
        return msgs ? { ...s, messages: msgs } : s;
      });
    });

    // The run paused for tool approval: nothing is executing until the user
    // decides, so live indicators come down and `running` reflects the truth
    // (reloads merge the paused turn from the durable approvals meanwhile).
    // The mappings stay: the approval decision RESUMES THE SAME run id, so a
    // later run.started on this id flips the session back to live seamlessly.
    ws.on(EV.runInterrupted, (p: { run_id: string }) => {
      if (taskRunsRef.current[p.run_id]) {
        updateTask(p.run_id, { status: 'input_required' });
        // High-signal, once per pause: background approvals are otherwise
        // easy to miss (the conversation itself stays quiet).
        toast.info('Task "' + (taskRunsRef.current[p.run_id].label || p.run_id.slice(0, 8)) + '" needs approval');
        return;
      }
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      delete streamBufsRef.current[p.run_id];
      delete reasoningBufsRef.current[p.run_id];
      // Keep liveStartedAt so the timer resumes from the original start when
      // the run continues after approval. running=false already hides the
      // live timer; clearing liveStartedAt would lose the original timestamp.
      updateSS(sid, s => ({
        ...s, streaming: '', reasoning: '', running: false, compacting: false,
        liveRunId: null, liveAgentName: null,
      }));
    });

    ws.on(EV.runAgentStart, (p: { run_id: string; agent_name?: string }) => {
      const sid = runMapRef.current[p.run_id];
      if (!sid || !p.agent_name) return;
      updateSS(sid, s => s.liveAgentName === p.agent_name ? s : { ...s, liveAgentName: p.agent_name || null });
    });

    // This connection fell behind and the server dropped events for it — for
    // this connection only; the run is unaffected. Without this the timeline
    // would be quietly missing whatever was dropped, which is indistinguishable
    // from content that never existed. Refetching is the cheap correct fix: the
    // persisted history is authoritative.
    ws.on(EV.runGap, (p: { run_id: string; dropped: number; last_good: number }) => {
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      console.warn(`dropped ${p.dropped} event(s) after seq ${p.last_good}; refetching`);
      reloadMessages(sid);
    });

    // Trouble the run survived. It arrives with the terminal event, so it is
    // recorded rather than animated: the point is the record afterwards.
    ws.on(EV.runDiagnostic, (p: RunDiagnostic & { run_id: string }) => {
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      updateSS(sid, s => ({ ...s, diagnostics: [...s.diagnostics, p] }));
    });

    ws.on(EV.runCompaction, (p: { run_id: string; phase: string }) => {
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      updateSS(sid, s => ({ ...s, compacting: p.phase === 'started' }));
    });

    // The completed agent switch becomes a part INSIDE the live turn: the turn
    // must stay the last message — every stream handler above and ChatView's
    // isLive check anchor on it, so a message appended after it would freeze
    // live rendering for the rest of the run. handoff_requested events (no
    // `to` yet) are preview noise and are skipped.
    ws.on(EV.runHandoff, (p: { run_id: string; from: string; to?: string }) => {
      if (taskRunsRef.current[p.run_id]) {
        if (p.to) {
          updateTaskView(p.run_id, v => {
            const msgs = appendHandoffPart(ensureLiveTurn(v.messages, p.run_id) || v.messages, p.from + ' → ' + p.to);
            return msgs ? { ...v, messages: msgs } : v;
          });
        }
        return;
      }
      const sid = runMapRef.current[p.run_id];
      if (!sid || !p.to) return;
      // Hub replays (reconnect) re-deliver run.handoff; the reducer dedups like
      // run.message / run.reasoning_item so a reconnect mid-run doesn't stack rows.
      updateSS(sid, s => {
        const msgs = appendHandoffPart(s.messages, p.from + ' → ' + p.to);
        return msgs ? { ...s, messages: msgs } : s;
      });
    });

    ws.on(EV.traceSpan, (p: { run_id: string; name: string; type?: string; span_id?: string; parent_id?: string; error?: string; started_at?: string; ended_at?: string; data?: Record<string, unknown> }) => {
      if (taskRunsRef.current[p.run_id]) {
        updateTaskView(p.run_id, v => {
          let duration = '';
          if (p.started_at && p.ended_at) {
            const ms = new Date(p.ended_at).getTime() - new Date(p.started_at).getTime();
            duration = ms < 1000 ? ms + 'ms' : (ms / 1000).toFixed(1) + 's';
          }
          const ev: TraceEvent = {
            kind: 'span', name: p.name, type: p.type || '',
            span_id: p.span_id, parent_id: p.parent_id,
            error: p.error, started_at: p.started_at, ended_at: p.ended_at,
            data: p.data || null, duration,
          };
          const idx = p.span_id ? v.traces.findIndex(e => e.span_id === p.span_id) : -1;
          const traces = idx >= 0 ? [...v.traces.slice(0, idx), ev, ...v.traces.slice(idx + 1)] : [...v.traces, ev];
          return { ...v, traces };
        });
        return;
      }
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      updateSS(sid, s => {
        const events = s.traceRuns[p.run_id] || [];
        let duration = '';
        if (p.started_at && p.ended_at) {
          const ms = new Date(p.ended_at).getTime() - new Date(p.started_at).getTime();
          duration = ms < 1000 ? ms + 'ms' : (ms / 1000).toFixed(1) + 's';
        }
        const ev: TraceEvent = {
          kind: 'span', name: p.name, type: p.type || '',
          span_id: p.span_id, parent_id: p.parent_id,
          error: p.error, started_at: p.started_at, ended_at: p.ended_at,
          data: p.data || null, duration,
        };
        // A span arrives twice: pending on start, full on end — upsert by id.
        const idx = p.span_id ? events.findIndex(e => e.span_id === p.span_id) : -1;
        const next = idx >= 0 ? [...events.slice(0, idx), ev, ...events.slice(idx + 1)] : [...events, ev];
        return { ...s, traceRuns: { ...s.traceRuns, [p.run_id]: next } };
      });
    });

    // On reconnect, any run that kept executing server-side may have advanced
    // or finished while we were away. Re-subscribe to still-live runs (the
    // hub replays buffered events) and reload persisted history so a run that
    // completed offline shows its result.
    ws.onReconnect = () => {
      // Task runs are not resubscribed (their terminal events may be gone from
      // the hub entirely) — re-pull the durable rows so statuses that changed
      // during the outage land in the chips.
      for (const sid of loadedRef.current) {
        (api.sessions.tasks(sid) as Promise<Array<{ task_id: string; parent_run_id?: string; label?: string; status?: string; attempt?: number; summary?: string; tool_call_id?: string; child_session_id?: string; created_at?: string; updated_at?: string }>>)
          .then(rows => {
            if (!rows || rows.length === 0) return;
            updateSS(sid, s => {
              const tasks = { ...s.tasks };
              for (const row of rows) {
                const cur = tasks[row.task_id];
                const status = (row.status || 'working') as TaskState['status'];
                tasks[row.task_id] = {
                  ...(cur || { taskId: row.task_id, label: row.label || '', toolCallId: row.tool_call_id }),
                  childSessionId: cur?.childSessionId || row.child_session_id,
                  parentRunId: cur?.parentRunId || row.parent_run_id,
                  status, summary: row.summary ?? cur?.summary,
                  createdAt: (row.created_at ? Date.parse(row.created_at) : undefined) || cur?.createdAt,
                  updatedAt: (row.updated_at ? Date.parse(row.updated_at) : undefined) || cur?.updatedAt,
                };
              }
              return { ...s, tasks };
            });
          }).catch(() => undefined);
      }
      for (const [sid, runId] of Object.entries(sessionRunRef.current)) {
        ws.send(EV.runSubscribe, { run_id: runId });
        reloadMessages(sid);
      }
    };

    ws.connect();
    return () => {
      ws.close();
      if (rafIdRef.current) {
        cancelAnimationFrame(rafIdRef.current);
        rafIdRef.current = 0;
      }
      rafPendingRef.current.clear();
    };
  }, [updateSS, reloadMessages, scheduleFrame]);

  // watchTask opens the Inspector's live view of a task: snapshot the child
  // session's persisted transcript + traces, then let the child-run event
  // interceptors stream the live tail into it. unwatchTask drops everything.
  const watchTask = useCallback((sid: string, taskId: string, childSessionId: string) => {
    taskWatchRef.current = { sid, taskId, childSessionId };
    taskViewBufRef.current = { text: '', reasoning: '' };
    updateSS(sid, s => ({ ...s, taskView: { taskId, childSessionId, messages: [], streaming: '', reasoning: '', traces: [], loaded: false } }));
    Promise.all([
      // fetchTimeline (not raw messages): a task paused on an approval keeps
      // its dangling tool call out of messages (persist boundary) — the
      // pending-approval merge is what puts the approval card in the view.
      fetchTimeline(childSessionId),
      (api.sessions.traces(childSessionId) as Promise<any[]>).catch(() => []),
    ]).then(([{ timeline }, traceRows]) => {
      if (taskWatchRef.current?.taskId !== taskId) return; // switched away meanwhile
      const traces: TraceEvent[] = [];
      for (const ev of traceRows || []) {
        if (ev.kind !== 'span') continue;
        let parsed: Record<string, unknown> | null = null;
        if (ev.data) { try { parsed = JSON.parse(ev.data); } catch (_e) { parsed = null; } }
        let duration = '';
        if (ev.started_at && ev.ended_at) {
          const ms = new Date(ev.ended_at).getTime() - new Date(ev.started_at).getTime();
          duration = ms < 1000 ? ms + 'ms' : (ms / 1000).toFixed(1) + 's';
        }
        traces.push({ kind: 'span', name: ev.name || '', type: ev.detail || '', span_id: ev.span_id, parent_id: ev.parent_id, error: ev.error, started_at: ev.started_at, ended_at: ev.ended_at, data: parsed, duration });
      }
      updateSS(sid, s => {
        if (!s.taskView || s.taskView.taskId !== taskId) return s;
        // Live spans that raced the fetch win (upsert by span id).
        const merged = [...traces];
        for (const live of s.taskView.traces) {
          const idx = live.span_id ? merged.findIndex(e => e.span_id === live.span_id) : -1;
          if (idx >= 0) merged[idx] = live; else merged.push(live);
        }
        // Snapshot wins: the child rows share the live turn's runId, so a
        // mergeLiveTail would drop the in-flight turn wholesale. Terminal
        // events refetch (refetchTaskView), which closes the gap for good.
        return { ...s, taskView: { ...s.taskView, messages: timeline, traces: merged, loaded: true } };
      });
    }).catch(() => {
      updateSS(sid, s => (s.taskView && s.taskView.taskId === taskId ? { ...s, taskView: { ...s.taskView, loaded: true } } : s));
    });
  }, [updateSS]);

  const unwatchTask = useCallback((sid: string) => {
    taskWatchRef.current = null;
    taskViewBufRef.current = { text: '', reasoning: '' };
    updateSS(sid, s => (s.taskView ? { ...s, taskView: null } : s));
  }, [updateSS]);

  // loadEarlier prepends the previous page of history.
  //
  // It rebuilds the timeline from ALL fetched entries rather than prepending
  // the new page's timeline to the old one: buildTimeline folds turns across
  // entries, so assembling the two halves separately splits whichever turn
  // straddles the page boundary into two turns that then render as two.
  //
  // beforeId is passed IN rather than read from state here. A state updater
  // runs when React processes the update, not at the call — reading the cursor
  // inside one left it undefined every time, so the fetch never started and the
  // button stayed on "Loading…" forever. The caller holds the state and knows
  // the cursor already.
  const loadEarlier = useCallback((sid: string, beforeId: number): void => {
    if (!sid || !beforeId || loadingMoreRef.current.has(sid)) return;
    const oldest = beforeId;
    loadingMoreRef.current.add(sid);
    updateSS(sid, s => ({ ...s, loadingMore: true }));
    (api.sessions.messages(sid, { limit: HISTORY_PAGE, beforeId: oldest }) as Promise<EntryView[]>)
      .then(older => {
        updateSS(sid, s => {
          const page = older || [];
          if (page.length === 0) return { ...s, hasMore: false, loadingMore: false };
          const entries = [...page, ...s.entries];
          // The live tail is re-merged because the rebuild only knows what is
          // persisted; an in-flight turn has nothing in the store yet. The
          // merge is scoped to the CURRENT live run — omitting liveRunId here
          // dropped the streaming turn when the user loaded earlier messages
          // mid-generation.
          const rebuilt = buildTimeline(entries) as SessionState['messages'];
          return {
            ...s,
            entries,
            messages: mergeLiveTail(rebuilt, s.messages, s.liveRunId),
            hasMore: page.length >= HISTORY_PAGE,
            loadingMore: false,
          };
        });
      })
      .catch(() => updateSS(sid, s => ({ ...s, loadingMore: false })))
      .finally(() => loadingMoreRef.current.delete(sid));
  }, [updateSS]);

  // forgetLoaded drops the "already fetched" mark so the next loadSession
  // re-reads from the server. A branch switch is the case that needs it: the
  // conversation changed shape server-side, and no local patch can express
  // "this is now a different branch". Bumping the generation invalidates every
  // timeline fetch already in flight — their responses describe the old path.
  const forgetLoaded = useCallback((sid: string) => {
    loadedRef.current.delete(sid);
    timelineGenRef.current[sid] = (timelineGenRef.current[sid] || 0) + 1;
    updateSS(sid, s => ({ ...s, loaded: false, entries: [], hasMore: false }));
  }, [updateSS]);

  return { wsRef, sessionRunRef, loadSession, deleteSession, loadEarlier, forgetLoaded, watchTask, unwatchTask };
}
