import type { AttachmentMeta } from '@/lib/attachments';
import { useCallback, useEffect, useRef, useState } from 'react';
import { WSClient } from '@/lib/ws';
import { EV, ERR, type InjectQueue, type RunDiagnostic, type SessionStatusEvent, type TaskRow } from '@/lib/protocol';
import { buildTimeline, type DisplayExtra, type EntryView, type TimelineEntry, type ToolCall } from '@/lib/timeline';
import {
  ensureLiveTurn, mergeLiveTail, appendInjected, appendMessageItem, appendReasoningItem, finalizeTurn,
  appendErrorPart, appendCancelledPart, appendToolCall, applyToolResult, syncTaskCard, appendToolProgress, appendHandoffPart, resolvePendingApprovals, supersedePendingApprovals,
  TERMINAL_TASK_STATUSES,
} from '@/lib/streamReducer';
import { api, clearToken } from '@/lib/api';
import { invalidate } from '@/lib/apiCache';
import { SESSION_LISTS } from '@/lib/sessionPages';
import { resyncAfterGap, type GapResync } from '@/lib/gapResync';
import { toast } from '@/lib/toast';
import { putBackInComposer } from '@/lib/composer';
import { ME_RELOAD } from '@/lib/me';
import {
  createTaskRouter, mergeTaskRows, seedTaskRows, withPendingTaskApprovals,
  type TaskState, type TaskViewState, type TaskRouter,
} from '@/lib/taskEvents';

import type { TraceEventData as TraceEvent } from '@/features/chat/TracePanel';

export { taskStateFromRow, taskRetryable } from '@/lib/taskEvents';
export type { TaskState, TaskViewState } from '@/lib/taskEvents';

// QueuedInput is a message this tab queued on a live run (POST
// /runs/:id/inject) and has not yet seen the run read.
export interface QueuedInput {
  clientMsgId: string;
  runId: string;
  text: string;
  queue: InjectQueue;
}

export interface SessionState {
  messages: TimelineEntry[];
  streaming: string;
  reasoning: string;
  running: boolean;
  compacting: boolean;
  // Trouble the current run survived (retries, a fallback model, a failed
  // compaction); cleared when a new run starts.
  diagnostics: RunDiagnostic[];
  traceRuns: Record<string, TraceEvent[]>;
  liveRunId: string | null;
  loaded: boolean;
  // Why the persisted timeline could not be loaded; cleared by the next attempt.
  loadError?: string;
  // The raw rows messages was built from, off-path attempts included: what
  // the trace panel labels a branched-away run from.
  entries: EntryView[];
  tasks: Record<string, TaskState>;
  // tasksLoaded is set once the durable task rows have been asked for — what
  // tells a task deep link "not here yet" from "not here".
  tasksLoaded: boolean;
  // Set when that fetch failed, so an empty list reads as "could not load";
  // cleared by a successful load.
  tasksError?: string;
  // The task currently inspected in the side panel, or null.
  taskView: TaskViewState | null;
  // What this tab queued on the live run and the run has not read yet, in the
  // order sent.
  queued: QueuedInput[];
}

// A fetched timeline: the assembled messages and the raw entries they came from.
interface FetchedTimeline {
  timeline: SessionState['messages'];
  entries: EntryView[];
}

export type UpdateSSFn = (sid: string, updater: (s: SessionState) => SessionState) => void;

export function defaultSS(): SessionState {
  return {
    messages: [], streaming: '', reasoning: '', running: false, compacting: false, diagnostics: [],
    traceRuns: {}, liveRunId: null, loaded: false,
    entries: [], tasks: {}, tasksLoaded: false, taskView: null, queued: [],
  };
}

// TraceRow is a stored span as GET /sessions/:id/traces returns it.
export interface TraceRow {
  run_id?: string; parent_run_id?: string; kind?: string; name?: string; data?: string; detail?: string;
  error?: string; span_id?: string; parent_id?: string; started_at?: string; ended_at?: string; payload_omitted?: boolean;
  attachments?: AttachmentMeta[];
}

function spanDuration(startedAt?: string, endedAt?: string): string {
  if (!startedAt || !endedAt) return '';
  const ms = new Date(endedAt).getTime() - new Date(startedAt).getTime();
  return ms < 1000 ? ms + 'ms' : (ms / 1000).toFixed(1) + 's';
}

// traceEventFromRow is a stored span in the panel's shape: data parsed
// (pre-JSON rows read as none), the duration computed once.
export function traceEventFromRow(ev: TraceRow): TraceEvent {
  let parsed: Record<string, unknown> = {};
  if (ev.data) {
    try { parsed = JSON.parse(ev.data); } catch (_e) { /* pre-JSON rows */ }
  }
  return {
    kind: 'span', name: ev.name || '', type: ev.detail || '',
    span_id: ev.span_id, parent_id: ev.parent_id, parent_run_id: ev.parent_run_id,
    error: ev.error, started_at: ev.started_at, ended_at: ev.ended_at,
    data: Object.keys(parsed).length > 0 ? parsed : null, attachments: ev.attachments,
    duration: spanDuration(ev.started_at, ev.ended_at), payloadOmitted: !!ev.payload_omitted,
  };
}

// A live trace.span event in the panel's shape.
interface TraceSpanEvent {
  run_id: string; parent_run_id?: string; name: string; type?: string; span_id?: string; parent_id?: string;
  error?: string; started_at?: string; ended_at?: string; data?: Record<string, unknown>; payload_omitted?: boolean;
  attachments?: AttachmentMeta[];
}

function traceEventFromLive(p: TraceSpanEvent): TraceEvent {
  return {
    kind: 'span', name: p.name, type: p.type || '',
    span_id: p.span_id, parent_id: p.parent_id, parent_run_id: p.parent_run_id,
    error: p.error, started_at: p.started_at, ended_at: p.ended_at,
    data: p.data || null, attachments: p.attachments,
    duration: spanDuration(p.started_at, p.ended_at), payloadOmitted: !!p.payload_omitted,
  };
}

// withSpanPayload puts one span's whole row into every trace group that holds
// it — the chat's and the inspected task's — replacing the summary.
export function withSpanPayload(runs: Record<string, TraceEvent[]>, runId: string, spanId: string, full: TraceEvent): Record<string, TraceEvent[]> {
  const events = runs[runId];
  if (!events) return runs;
  const idx = events.findIndex(e => e.span_id === spanId);
  if (idx < 0) return runs;
  const cur = events[idx];
  const next = { ...cur, data: full.data ?? cur.data, attachments: full.attachments ?? cur.attachments, payloadOmitted: false };
  return { ...runs, [runId]: [...events.slice(0, idx), next, ...events.slice(idx + 1)] };
}

// SessionEvents is what the socket tells the app about a session beyond its run state;
// read through a ref per event, so the socket is not rebuilt when a callback changes.
export interface SessionEvents {
  // The conversation on screen: what a reconnect re-reads at once.
  activeSession: () => string | null;
  onTitleUpdated: (sessionId: string, title: string) => void;
  onProjectBound: (sessionId: string, projectId: string) => void;
  // The server's word on a session's status; null drops every one heard so far
  // (the refetched list answers).
  onStatus: (status: SessionStatusEvent | null) => void;
}

export function useAgentSocket(updateSSRaw: UpdateSSFn, events: SessionEvents) {
  const wsRef = useRef<WSClient | null>(null);
  const eventsRef = useRef(events);
  eventsRef.current = events;
  // Optimistic true: the indicator marks a LOST connection, not a pending one.
  const [connected, setConnected] = useState(true);
  // Sessions deleted in this page: a late event of the delete cascade, or a
  // fetch in flight when the session went, must not rebuild one — every write
  // goes through updateSS.
  const deletedRef = useRef<Set<string>>(new Set());
  const updateSS = useCallback<UpdateSSFn>((sid, fn) => {
    if (deletedRef.current.has(sid)) return;
    updateSSRaw(sid, fn);
  }, [updateSSRaw]);
  const runMapRef = useRef<Record<string, string>>({});
  const sessionRunRef = useRef<Record<string, string>>({});
  const streamBufsRef = useRef<Record<string, string>>({});
  const reasoningBufsRef = useRef<Record<string, string>>({});
  // Runs whose delta preview was dropped for a replay (gap resync, reconnect):
  // deltas are ignored until the next complete item, since the envelope carries
  // no sequence number to tell a replayed delta from a fresh one.
  const mutedRunsRef = useRef<Set<string>>(new Set());
  // Per-run ids of completed message/reasoning items already folded in: hub replays
  // re-deliver them, and deduping by id (not text) keeps a genuinely repeated message.
  const appendedItemsRef = useRef<Record<string, Set<string>>>({});
  // Each run's last resync after a gap (see the run.gap handler): when, and
  // from which cursor, so a range the ring has evicted is asked for once.
  const gapResyncRef = useRef<Record<string, GapResync>>({});
  const loadedRef = useRef<Set<string>>(new Set());
  // Sessions whose persisted traces have been pulled (see loadTraces).
  const tracesLoadedRef = useRef<Set<string>>(new Set());
  // Per-session timeline generation, bumped by forgetLoaded (a branch move): a fetch
  // launched before the bump describes an abandoned path and is dropped.
  const timelineGenRef = useRef<Record<string, number>>({});

  // The queued inputs per session: the ref is what the event handlers read
  // and write at once; SessionState.queued mirrors it for the view.
  const queuedRef = useRef<Record<string, QueuedInput[]>>({});
  const setQueued = useCallback((sid: string, next: QueuedInput[]) => {
    queuedRef.current[sid] = next;
    updateSS(sid, s => ({ ...s, queued: next }));
  }, [updateSS]);
  const queueInput = useCallback((sid: string, item: QueuedInput) => {
    setQueued(sid, [...(queuedRef.current[sid] || []), item]);
  }, [setQueued]);
  // dropQueued withdraws one queued input; false when the run already read it
  // or its end already returned it.
  const dropQueued = useCallback((sid: string, clientMsgId: string): boolean => {
    const cur = queuedRef.current[sid] || [];
    const next = cur.filter(q => q.clientMsgId !== clientMsgId);
    if (next.length === cur.length) return false;
    setQueued(sid, next);
    return true;
  }, [setQueued]);

  // Coalesce delta updates (run.step / run.reasoning) to one setState per animation
  // frame per key; the buffers accumulate in refs, so only renders are batched.
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
  // the stored entries, with a durable pending approval's tool calls merged
  // into the turn they belong to.
  const fetchTimeline = useCallback(async (sid: string): Promise<FetchedTimeline> => {
    type PendingApproval = { run_id: string; user_input?: string; task_id?: string; tool_calls?: Array<{ tool_call_id: string; tool_name: string; arguments: string }> };
    const [msgs, pendingAll] = await Promise.all([
      api.sessions.messages(sid) as Promise<EntryView[]>,
      (api.sessions.approvals(sid) as Promise<PendingApproval[]>).catch(() => [] as PendingApproval[]),
    ]);
    // A background task's approval surfaces on its chip, never in the chat timeline.
    const taskPending = (pendingAll || []).filter(p => p.task_id);
    if (taskPending.length > 0) updateSS(sid, s => withPendingTaskApprovals(s, taskPending));
    const entries = msgs || [];
    // A pending approval whose run has off-path entries belongs to a
    // branched-away attempt: kept server-side, not rebuilt here (switching back
    // re-admits it) — invariant 19.
    const offPathRuns = new Set(entries.filter(e => e.run_id && e.on_path === false).map(e => e.run_id));
    const pending = (pendingAll || []).filter(p => !p.task_id && !offPathRuns.has(p.run_id));
    const timeline = buildTimeline(entries);
    if (!pending || pending.length === 0) return { entries, timeline };
    const seen = new Set<string>();
    for (const m of timeline) {
      if (m.role !== 'turn') continue;
      for (const part of m.parts) {
        if (part.type === 'tools') for (const tc of part.toolCalls) seen.add(tc.tool_call_id);
      }
    }
    const toolCalls: ToolCall[] = pending.flatMap(p => (p.tool_calls || []).map(tc => ({
      tool_call_id: tc.tool_call_id, tool_name: tc.tool_name, arguments: tc.arguments,
      output: null, status: null, needs_approval: true,
    }))).filter(tc => !seen.has(tc.tool_call_id));
    if (toolCalls.length === 0) return { entries, timeline };
    const runId = pending[0].run_id;
    const userInput = pending[0].user_input || '';
    // The synthesized rows have no row id: a pending approval was never persisted.
    const out = [...timeline];

    // Merge the pending calls into the run's turn when the timeline already holds it;
    // only when nothing for this run is persisted is the paused turn rebuilt whole.
    let lastTurnIdx = -1;
    for (let i = out.length - 1; i >= 0; i--) {
      const m = out[i];
      if (m.role === 'turn') { if (m.runId === runId) lastTurnIdx = i; break; }
      if (m.role === 'user') break;
    }
    const turn = lastTurnIdx >= 0 ? out[lastTurnIdx] : null;
    if (turn && turn.role === 'turn') {
      const parts = [...turn.parts];
      const lastPart = parts[parts.length - 1];
      if (lastPart?.type === 'tools') parts[parts.length - 1] = { ...lastPart, toolCalls: [...lastPart.toolCalls, ...toolCalls] };
      else parts.push({ type: 'tools', toolCalls });
      out[lastTurnIdx] = { ...turn, parts };
      return { entries, timeline: out };
    }
    const hasUser = out.some(m => m.role === 'user' && (m.runId === runId || (userInput && m.content === userInput)));
    if (userInput && !hasUser) out.push({ role: 'user', content: userInput, runId, messageId: undefined });
    out.push({ role: 'turn', parts: [{ type: 'tools', toolCalls }], runId, messageId: undefined });
    return { entries, timeline: out };
  }, [updateSS]);

  // The task router lives for the hook's lifetime; it reads the latest
  // callbacks through the ref on every call.
  const routerDepsRef = useRef({ updateSS, fetchTimeline, scheduleFrame });
  routerDepsRef.current = { updateSS, fetchTimeline, scheduleFrame };
  const tasksRef = useRef<TaskRouter | null>(null);
  if (!tasksRef.current) {
    tasksRef.current = createTaskRouter(() => ({
      ...routerDepsRef.current,
      sessionOfRun: runId => runMapRef.current[runId],
      isDeleted: sid => deletedRef.current.has(sid),
    }));
  }
  const tasks = tasksRef.current;

  const reloadMessages = useCallback((sid: string) => {
    const gen = timelineGenRef.current[sid] || 0;
    fetchTimeline(sid).then(({ timeline, entries }) => {
      // A branch move happened while this fetch was in flight: its own reload
      // owns the state.
      if ((timelineGenRef.current[sid] || 0) !== gen) return;
      // Never clobber a running session: mid-resume the paused turn is in neither
      // messages nor approvals, and every terminal event reloads anyway.
      updateSS(sid, s => s.running ? s : { ...s, messages: timeline, entries });
    }).catch((e: { status?: number }) => {
      // A session gone (deleted here or elsewhere: 404) has nothing to refresh.
      if (deletedRef.current.has(sid) || e?.status === 404) return;
      toast.error('Could not refresh the session — reopen it to retry');
    });
  }, [fetchTimeline, updateSS]);

  // loadTimeline fetches a session's persisted timeline once (loadedRef):
  // merged under the live tail when the session is already shown, taken whole
  // otherwise. A failure rolls the mark back.
  const loadTimeline = useCallback((sid: string): Promise<void> => {
    if (!sid || loadedRef.current.has(sid)) return Promise.resolve();
    loadedRef.current.add(sid);
    const gen = timelineGenRef.current[sid] || 0;
    updateSS(sid, s => (s.loadError ? { ...s, loadError: undefined } : s));
    return fetchTimeline(sid).then(({ timeline, entries }) => {
      // Superseded by a later branch move's own reload (see reloadMessages).
      if ((timelineGenRef.current[sid] || 0) !== gen) return;
      updateSS(sid, s => s.loaded
        ? { ...s, messages: mergeLiveTail(timeline, s.messages, s.liveRunId), entries }
        : { ...s, messages: timeline, entries, loaded: true });
    }).catch((err: Error) => {
      loadedRef.current.delete(sid);
      updateSS(sid, s => ({ ...s, loadError: err?.message || 'Could not load the session' }));
      throw err;
    });
  }, [fetchTimeline, updateSS]);

  // loadTasks seeds the task list from the durable rows; live task-run events win per
  // task id. A failure is the list's own state — invariant 79.
  const loadTasks = useCallback((sid: string): void => {
    (api.sessions.tasks(sid) as Promise<TaskRow[]>)
      .then(rows => {
        if (!rows || rows.length === 0) {
          updateSS(sid, s => s.tasksLoaded && !s.tasksError ? s : { ...s, tasksLoaded: true, tasksError: undefined });
          return;
        }
        updateSS(sid, s => ({ ...seedTaskRows(s, rows), tasksError: undefined }));
      }).catch((e: { status?: number; message?: string }) => {
        if (deletedRef.current.has(sid) || e?.status === 404) return;
        // Loaded-with-error, not loaded-empty: the panel must not read it as
        // "no tasks".
        updateSS(sid, s => ({ ...s, tasksLoaded: true, tasksError: e?.message || 'request failed' }));
      });
  }, [updateSS]);

  const loadSession = useCallback((sid: string): Promise<void> => {
    if (!sid || loadedRef.current.has(sid)) return Promise.resolve();
    // Loaded again is not deleted: a session transferred away and back takes
    // writes again.
    deletedRef.current.delete(sid);
    const msgP = loadTimeline(sid);
    loadTasks(sid);
    return msgP;
  }, [loadTimeline, loadTasks]);

  // fetchTraces pulls the session's span summary (payloads stay lazy, see
  // loadSpanPayload). Per run id the live group wins unless fetchedWins (a
  // resync after an outage).
  const fetchTraces = useCallback((sid: string, fetchedWins: boolean) => {
    (api.sessions.traces(sid, { summary: true }) as Promise<TraceRow[] | null>).then(events => {
      if (!events || events.length === 0) return;
      const runs: Record<string, TraceEvent[]> = {};
      for (const ev of events) {
        // Spans are the sole trace source; kind=hook rows are ignored.
        if (ev.kind !== 'span') continue;
        const rid = ev.run_id || 'unknown';
        if (!runs[rid]) runs[rid] = [];
        runs[rid].push(traceEventFromRow(ev));
      }
      updateSS(sid, s => ({ ...s, traceRuns: fetchedWins ? { ...s.traceRuns, ...runs } : { ...runs, ...s.traceRuns } }));
    }).catch(() => {
      // Roll back the mark so the next lens open retries.
      tracesLoadedRef.current.delete(sid);
    });
  }, [updateSS]);

  // loadTraces backfills a session's traces once, on load: the chat labels each turn
  // with its run span's duration, so the data cannot wait for a lens to open.
  const loadTraces = useCallback((sid: string) => {
    if (!sid || tracesLoadedRef.current.has(sid)) return;
    tracesLoadedRef.current.add(sid);
    fetchTraces(sid, false);
  }, [fetchTraces]);

  // loadSpanPayload fetches one span whole and folds it into the chat's trace
  // groups and the inspected task's; spanSessionId is the session whose stored
  // rows hold it. Rejects while the span is still live.
  const loadSpanPayload = useCallback(async (sid: string, spanSessionId: string, runId: string, spanId: string): Promise<void> => {
    const row = await (api.sessions.traceSpan(spanSessionId, spanId) as Promise<TraceRow>);
    const full = traceEventFromRow(row);
    updateSS(sid, s => {
      const traceRuns = withSpanPayload(s.traceRuns, runId, spanId, full);
      const viewRuns = s.taskView ? withSpanPayload(s.taskView.traceRuns, runId, spanId, full) : null;
      const taskView = s.taskView && viewRuns && viewRuns !== s.taskView.traceRuns ? { ...s.taskView, traceRuns: viewRuns } : s.taskView;
      return traceRuns === s.traceRuns && taskView === s.taskView ? s : { ...s, traceRuns, taskView };
    });
  }, [updateSS]);

  // deleteSession forgets a session the server deleted: its load marks and
  // every run still routed to it, so the cascade's own late terminal events
  // cannot rebuild it as a ghost.
  const deleteSession = useCallback((deletedId: string) => {
    deletedRef.current.add(deletedId);
    loadedRef.current.delete(deletedId);
    tracesLoadedRef.current.delete(deletedId);
    for (const [runId, sid] of Object.entries(runMapRef.current)) {
      if (sid !== deletedId) continue;
      delete runMapRef.current[runId];
      delete streamBufsRef.current[runId];
      delete reasoningBufsRef.current[runId];
      delete appendedItemsRef.current[runId];
      delete gapResyncRef.current[runId];
      mutedRunsRef.current.delete(runId);
    }
    delete sessionRunRef.current[deletedId];
    delete queuedRef.current[deletedId];
    tasks.forgetSession(deletedId);
  }, [tasks]);

  useEffect(() => {
    const ws = new WSClient();
    wsRef.current = ws;

    // The socket kept closing before authenticating: the token is rejected.
    // Clear it and drop to the login screen (as the REST 401 path does) instead
    // of reconnecting forever.
    ws.onAuthFail = () => {
      clearToken();
      window.dispatchEvent(new Event('auth:logout'));
      toast.error('Session expired — please sign in again');
    };

    ws.onStatus = setConnected;

    // dropRunRefs clears a terminal run's routing bookkeeping (safe no-ops for
    // refs it never acquired).
    const dropRunRefs = (runId: string) => {
      const sid = runMapRef.current[runId];
      delete streamBufsRef.current[runId];
      delete reasoningBufsRef.current[runId];
      delete appendedItemsRef.current[runId];
      delete gapResyncRef.current[runId];
      delete runMapRef.current[runId];
      mutedRunsRef.current.delete(runId);
      if (sid && sessionRunRef.current[sid] === runId) delete sessionRunRef.current[sid];
    };

    // putBackUnread returns what was queued on an ended run and never read to
    // the box it was typed in — nothing a person typed is dropped silently.
    const putBackUnread = (sid: string, runId: string) => {
      const all = queuedRef.current[sid] || [];
      const unread = all.filter(q => q.runId === runId);
      if (unread.length === 0) return;
      setQueued(sid, all.filter(q => q.runId !== runId));
      putBackInComposer(sid, eventsRef.current.activeSession() === sid, unread.map(q => q.text).join('\n'));
      toast.info('Unread queued messages were put back');
    };

    // resubscribe asks the hub to replay a chat run from `fromSeq` and clears
    // its delta preview. Over a live socket the old subscription still pushes
    // until the hub swaps it, so a gap resync also mutes deltas.
    const resubscribe = (runId: string, fromSeq?: number, mute = true) => {
      ws.send(EV.runSubscribe, fromSeq === undefined ? { run_id: runId } : { run_id: runId, from_seq: fromSeq });
      streamBufsRef.current[runId] = '';
      reasoningBufsRef.current[runId] = '';
      if (mute) mutedRunsRef.current.add(runId);
      const sid = runMapRef.current[runId];
      if (sid) updateSS(sid, s => (s.streaming || s.reasoning ? { ...s, streaming: '', reasoning: '' } : s));
    };

    ws.on(EV.runStarted, (p: { session_id?: string; run_id: string; input?: string; attachments?: AttachmentMeta[]; parent_session_id?: string; parent_run_id?: string; task_id?: string; kind?: string; tool_call_id?: string; label?: string; attempt?: number; max_attempts?: number }) => {
      // A background task run (a sub-agent's, a workflow step's) is the router's alone.
      if (tasks.runStarted(p)) return;
      const sid = p.session_id;
      if (!sid || deletedRef.current.has(sid)) return;
      runMapRef.current[p.run_id] = sid;
      sessionRunRef.current[sid] = p.run_id;
      streamBufsRef.current[p.run_id] = '';
      reasoningBufsRef.current[p.run_id] = '';
      // Keep the dedup set across a replay/resume of the same run id.
      if (!appendedItemsRef.current[p.run_id]) appendedItemsRef.current[p.run_id] = new Set();
      // loadedRef is NOT marked here: run events reach every browser (invariant
      // 14), so a watching one still needs loadSession's fetch, whose merge
      // keeps the live entries.
      updateSS(sid, s => {
        // A hub replay re-delivers run.started (the reducer returns null); a
        // newer run's start also settles an older run's pending approval cards
        // — invariant 19.
        const appended = ensureLiveTurn(s.messages, p.run_id, p.input, p.attachments) || s.messages;
        return {
          ...s, running: true, compacting: false, diagnostics: [], liveRunId: p.run_id,
          loaded: true,
          messages: supersedePendingApprovals(appended, p.run_id) || appended,
          traceRuns: { ...s.traceRuns, [p.run_id]: s.traceRuns[p.run_id] || [] },
        };
      });
    });

    ws.on(EV.taskUpdated, (p: TaskRow) => tasks.taskUpdated(p));

    ws.on(EV.runStep, (p: { run_id: string; delta: string }) => {
      if (tasks.step(p)) return;
      const sid = runMapRef.current[p.run_id];
      if (!sid || mutedRunsRef.current.has(p.run_id)) return;
      streamBufsRef.current[p.run_id] = (streamBufsRef.current[p.run_id] || '') + p.delta;
      scheduleFrame('step:' + p.run_id, () => {
        const buf = streamBufsRef.current[p.run_id];
        if (buf !== undefined) updateSS(sid, s => ({ ...s, streaming: buf }));
      });
    });

    ws.on(EV.runReasoning, (p: { run_id: string; delta: string }) => {
      if (tasks.reasoning(p)) return;
      const sid = runMapRef.current[p.run_id];
      if (!sid || mutedRunsRef.current.has(p.run_id)) return;
      reasoningBufsRef.current[p.run_id] = (reasoningBufsRef.current[p.run_id] || '') + p.delta;
      scheduleFrame('reasoning:' + p.run_id, () => {
        const buf = reasoningBufsRef.current[p.run_id];
        if (buf !== undefined) updateSS(sid, s => ({ ...s, reasoning: buf }));
      });
    });

    // One completed assistant message, authoritative over the run.step deltas that
    // previewed it: the delta buffer is dropped so nothing appends the same text again.
    ws.on(EV.runMessage, (p: { run_id: string; text: string; item_id?: string }) => {
      if (tasks.message(p)) return;
      const sid = runMapRef.current[p.run_id];
      if (!sid || !p.text) return;
      mutedRunsRef.current.delete(p.run_id);
      streamBufsRef.current[p.run_id] = '';
      const seen = appendedItemsRef.current[p.run_id] || (appendedItemsRef.current[p.run_id] = new Set());
      // Hub replays re-deliver run.message: dedup by item id, by text only for
      // backends that send none.
      if (p.item_id && seen.has(p.item_id)) { updateSS(sid, s => ({ ...s, streaming: '' })); return; }
      updateSS(sid, s => {
        const msgs = appendMessageItem(s.messages, p.text, !p.item_id);
        return msgs ? { ...s, messages: msgs, streaming: '' } : { ...s, streaming: '' };
      });
      if (p.item_id) seen.add(p.item_id);
    });

    // One completed reasoning block, authoritative over the run.reasoning
    // deltas: freezing it as a part scopes the live preview to the current
    // turn, and is the only signal on backends that stream no deltas.
    ws.on(EV.runReasoningItem, (p: { run_id: string; text: string; item_id?: string }) => {
      if (tasks.reasoningItem(p)) return;
      const sid = runMapRef.current[p.run_id];
      if (!sid || !p.text) return;
      mutedRunsRef.current.delete(p.run_id);
      reasoningBufsRef.current[p.run_id] = '';
      const seen = appendedItemsRef.current[p.run_id] || (appendedItemsRef.current[p.run_id] = new Set());
      // Hub replays re-deliver run.reasoning_item: dedup by item id, by text
      // only when the backend sends none.
      if (p.item_id && seen.has(p.item_id)) { updateSS(sid, s => ({ ...s, reasoning: '' })); return; }
      updateSS(sid, s => {
        const msgs = appendReasoningItem(s.messages, p.text, !p.item_id);
        return msgs ? { ...s, messages: msgs, reasoning: '' } : { ...s, reasoning: '' };
      });
      if (p.item_id) seen.add(p.item_id);
    });

    ws.on(EV.runOutput, (p: { run_id: string; final_output?: string }) => {
      if (tasks.output(p)) { dropRunRefs(p.run_id); return; }
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      const text = p.final_output || streamBufsRef.current[p.run_id] || '';
      const thinking = reasoningBufsRef.current[p.run_id] || '';
      dropRunRefs(p.run_id);
      updateSS(sid, s => {
        const msgs = finalizeTurn(s.messages, text, thinking);
        return { ...s, messages: msgs || s.messages, streaming: '', reasoning: '', running: false, compacting: false, liveRunId: null };
      });
      putBackUnread(sid, p.run_id);
      reloadMessages(sid);
    });

    // The run read a queued input: the turn so far ends, the input shows as a
    // user message, a new turn follows (invariant 16). This tab's oldest queued
    // input with that text leaves the queue.
    ws.on(EV.runInjected, (p: { run_id: string; input: string; index: number }) => {
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      const seen = appendedItemsRef.current[p.run_id] || (appendedItemsRef.current[p.run_id] = new Set());
      if (seen.has('inj:' + p.index)) return;
      seen.add('inj:' + p.index);
      streamBufsRef.current[p.run_id] = '';
      reasoningBufsRef.current[p.run_id] = '';
      const all = queuedRef.current[sid] || [];
      const at = all.findIndex(q => q.runId === p.run_id && q.text === p.input);
      if (at >= 0) queuedRef.current[sid] = [...all.slice(0, at), ...all.slice(at + 1)];
      const queued = queuedRef.current[sid] || [];
      updateSS(sid, s => {
        const msgs = appendInjected(s.messages, p.run_id, p.input, p.index);
        return { ...s, messages: msgs || s.messages, queued, streaming: '', reasoning: '' };
      });
    });

    ws.on(EV.runError, (p: { run_id?: string; session_id?: string; code?: string; message: string; guardrail?: string; stage?: string }) => {
      if (p.run_id && tasks.error({ run_id: p.run_id, message: p.message })) { dropRunRefs(p.run_id); return; }
      // The session already has a live run (a double-send from another tab): a
      // toast, not a terminal error. The rejected send's optimistic bubble (the
      // newest with a clientMsgId and no run/row id) rolls back.
      if (p.code === ERR.sessionBusy) {
        toast.error(p.message || 'Session already has an active run');
        const sid = p.session_id || (p.run_id ? runMapRef.current[p.run_id] : undefined);
        if (sid) {
          updateSS(sid, s => {
            for (let i = s.messages.length - 1; i >= 0; i--) {
              const m = s.messages[i];
              if (m.role === 'user' && m.clientMsgId && m.messageId === undefined && m.runId === undefined) {
                return { ...s, messages: s.messages.slice(0, i).concat(s.messages.slice(i + 1)) };
              }
            }
            return s;
          });
        }
        return;
      }
      // An approve/reject the server refused: the optimistic card status was
      // never rolled back, so rebuild the paused turn from the durable approval
      // row. A refusal with no ids means the session on screen.
      if (p.code === ERR.approvalFailed) {
        toast.error(p.message || 'Approval failed');
        const sid = p.session_id || (p.run_id ? runMapRef.current[p.run_id] : undefined) || eventsRef.current.activeSession();
        if (sid) reloadMessages(sid);
        return;
      }
      // The run we tried to resubscribe expired server-side: drop the stale
      // mapping, fall back to persisted history.
      if (p.code === ERR.runNotFound) {
        const staleSid = p.run_id ? runMapRef.current[p.run_id] : undefined;
        if (p.run_id) dropRunRefs(p.run_id);
        if (staleSid) {
          updateSS(staleSid, s => ({ ...s, streaming: '', reasoning: '', running: false, compacting: false, liveRunId: null }));
          if (p.run_id) putBackUnread(staleSid, p.run_id);
          reloadMessages(staleSid);
        }
        return;
      }
      // Failures before run.started carry session_id instead of a mapped run id.
      const sid = (p.run_id && runMapRef.current[p.run_id]) || p.session_id;
      if (!sid) {
        toast.error(p.message || 'Run failed');
        return;
      }
      const rid = p.run_id || '';
      const remaining = streamBufsRef.current[rid] || '';
      const thinking = reasoningBufsRef.current[rid] || '';
      dropRunRefs(rid);
      delete sessionRunRef.current[sid];
      // A guardrail block carries the guardrail name + stage, for a distinct
      // "blocked" card.
      const errPart = p.code === ERR.guardrailTripwire
        ? { type: 'error' as const, content: p.message, code: p.code, guardrail: p.guardrail, stage: p.stage }
        : { type: 'error' as const, content: p.message, code: p.code };
      updateSS(sid, s => ({
        ...s, messages: appendErrorPart(s.messages, errPart, thinking, remaining),
        streaming: '', reasoning: '', running: false, compacting: false, liveRunId: null,
      }));
      if (rid) putBackUnread(sid, rid);
      // A guardrail block keeps its optimistic retracted-answer view, which a
      // reload would drop — invariant 17.
      if (p.code !== ERR.guardrailTripwire) reloadMessages(sid);
    });

    ws.on(EV.runCancelled, (p: { run_id?: string; reason?: string }) => {
      if (p?.run_id && tasks.cancelled({ run_id: p.run_id })) { dropRunRefs(p.run_id); return; }
      const rid = p?.run_id;
      const sid = rid ? runMapRef.current[rid] : null;
      if (!sid || !rid) return;
      const remaining = streamBufsRef.current[rid] || '';
      const thinking = reasoningBufsRef.current[rid] || '';
      dropRunRefs(rid);
      const reason = p.reason || 'stopped';
      // The marker shows at once rather than waiting on the reload (which the
      // next run's start can skip). A paused run's pending cards resolve to not
      // run; superseded, they say so and no marker follows.
      updateSS(sid, s => {
        let msgs = resolvePendingApprovals(s.messages, rid, reason) || s.messages;
        if (reason !== 'superseded') msgs = appendCancelledPart(msgs, thinking, remaining) || msgs;
        // A newer run may already own the session (the abandoned one's cancel
        // can land late): only the live run stands it down.
        if (s.liveRunId && s.liveRunId !== rid) return { ...s, messages: msgs };
        return { ...s, messages: msgs, streaming: '', reasoning: '', running: false, compacting: false, liveRunId: null };
      });
      putBackUnread(sid, rid);
      reloadMessages(sid);
    });

    ws.on(EV.runToolCall, (p: { run_id: string; tool_call_id: string; tool_name: string; arguments: string; needs_approval?: boolean }) => {
      if (tasks.toolCall(p)) return;
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      // A hub replay delivers the call again: it patches the existing card, and must
      // neither flush the streamed text twice nor blank the in-flight preview.
      const seen = appendedItemsRef.current[p.run_id] || (appendedItemsRef.current[p.run_id] = new Set());
      const replayed = seen.has('tc:' + p.tool_call_id);
      seen.add('tc:' + p.tool_call_id);
      const flushed = replayed ? '' : (streamBufsRef.current[p.run_id] || '');
      if (!replayed) streamBufsRef.current[p.run_id] = '';
      // needs_approval is undefined, not false: a reload only marks pending
      // calls — invariant 16.
      const tc = { tool_call_id: p.tool_call_id, tool_name: p.tool_name, arguments: p.arguments, needs_approval: p.needs_approval || undefined, status: null as string | null, output: null as string | null };
      updateSS(sid, s => {
        let msgs = appendToolCall(s.messages, tc, flushed);
        // The task's terminal fold may have run before this card existed
        // (parent and task runs have no cross-run ordering): fold the outcome
        // held in s.tasks onto the card — invariant 21.
        if (msgs) {
          for (const t of Object.values(s.tasks)) {
            if (t.toolCallId === p.tool_call_id && TERMINAL_TASK_STATUSES.has(t.status)) {
              msgs = syncTaskCard(msgs, p.tool_call_id, { id: t.taskId, label: t.label, status: t.status, summary: t.summary, attempt: t.attempt }) ?? msgs;
              break;
            }
          }
        }
        return msgs ? { ...s, messages: msgs, streaming: replayed ? s.streaming : '' } : s;
      });
    });

    // Live output of a tool still running; it accumulates on the card until
    // run.tool_result replaces it.
    ws.on(EV.runToolProgress, (p: { run_id: string; call_id: string; delta: string; renderer?: string }) => {
      if (tasks.toolProgress(p)) return;
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      updateSS(sid, s => {
        const msgs = appendToolProgress(s.messages, p.call_id, p.delta, p.renderer);
        return msgs ? { ...s, messages: msgs } : s;
      });
    });

    ws.on(EV.runToolResult, (p: { run_id: string; tool_call_id: string; output: string; title?: string; summary?: string; renderer?: string; is_error?: boolean; extra?: DisplayExtra }) => {
      if (tasks.toolResult(p)) return;
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      updateSS(sid, s => {
        const msgs = applyToolResult(s.messages, p.tool_call_id, p.output, p);
        return msgs ? { ...s, messages: msgs } : s;
      });
    });

    // The run paused for approval: indicators come down and `running` is false
    // (reloads merge the paused turn from the durable approvals). The mappings
    // stay: the decision resumes the SAME run id.
    ws.on(EV.runInterrupted, (p: { run_id: string }) => {
      if (tasks.interrupted(p)) return;
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      delete streamBufsRef.current[p.run_id];
      delete reasoningBufsRef.current[p.run_id];
      mutedRunsRef.current.delete(p.run_id);
      updateSS(sid, s => ({
        ...s, streaming: '', reasoning: '', running: false, compacting: false,
        liveRunId: null,
      }));
    });

    // This connection fell behind: re-subscribe from the last good cursor and
    // the hub replays — invariant 14. Chat runs only; a task run's inspector
    // view keeps no item ids to dedup by.
    ws.on(EV.runGap, (p: { run_id: string; dropped: number; last_good: number }) => {
      if (!runMapRef.current[p.run_id]) return;
      const now = Date.now();
      if (!resyncAfterGap(gapResyncRef.current[p.run_id], p.last_good, now)) return;
      gapResyncRef.current[p.run_id] = { at: now, cursor: p.last_good };
      console.warn(`dropped ${p.dropped} event(s) after seq ${p.last_good}; resyncing from the hub's replay`);
      resubscribe(p.run_id, p.last_good);
    });

    // Trouble the run survived; it arrives with the terminal event, so it is
    // recorded, not animated.
    ws.on(EV.runDiagnostic, (p: RunDiagnostic & { run_id: string }) => {
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      // A hub replay delivers it again: the same record twice is one record.
      updateSS(sid, s => s.diagnostics.some(d => d.type === p.type && d.code === p.code && d.message === p.message)
        ? s
        : { ...s, diagnostics: [...s.diagnostics, p] });
    });

    ws.on(EV.runCompaction, (p: { run_id: string; phase: string }) => {
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      updateSS(sid, s => ({ ...s, compacting: p.phase === 'started' }));
    });

    // The completed agent switch becomes a part INSIDE the live turn, which
    // must stay the last message (every stream handler and ChatView's isLive
    // anchor on it). handoff_requested events (no `to`) are skipped.
    ws.on(EV.runHandoff, (p: { run_id: string; from: string; to?: string; from_id?: string; to_id?: string }) => {
      const handoff = { from: p.from, to: p.to || '', fromId: p.from_id, toId: p.to_id };
      if (tasks.handoff(p, handoff)) return;
      const sid = runMapRef.current[p.run_id];
      if (!sid || !p.to) return;
      // A hub replay re-delivers run.handoff; the reducer drops a part already
      // on the turn.
      updateSS(sid, s => {
        const msgs = appendHandoffPart(s.messages, handoff);
        return msgs ? { ...s, messages: msgs } : s;
      });
    });

    ws.on(EV.traceSpan, (p: TraceSpanEvent) => {
      const ev = traceEventFromLive(p);
      if (tasks.traceSpan(p.run_id, ev)) return;
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      updateSS(sid, s => {
        const events = s.traceRuns[p.run_id] || [];
        // A span arrives twice: pending on start, full on end — upsert by id.
        const idx = p.span_id ? events.findIndex(e => e.span_id === p.span_id) : -1;
        const next = idx >= 0 ? [...events.slice(0, idx), ev, ...events.slice(idx + 1)] : [...events, ev];
        return { ...s, traceRuns: { ...s.traceRuns, [p.run_id]: next } };
      });
    });

    ws.on(EV.sessionTitleUpdated, (p: { session_id?: string; title?: string }) => {
      invalidate(SESSION_LISTS);
      if (p?.session_id && typeof p.title === 'string') eventsRef.current.onTitleUpdated(p.session_id, p.title);
    });

    ws.on(EV.sessionProjectBound, (p: { session_id?: string; project_id?: string }) => {
      if (p?.session_id && p.project_id) eventsRef.current.onProjectBound(p.session_id, p.project_id);
    });

    ws.on(EV.sessionStatus, (p: SessionStatusEvent) => {
      if (p?.session_id && p.status) eventsRef.current.onStatus(p);
    });

    // resyncSessions repairs what an outage may have moved: the session on screen now,
    // every other loaded one on its next select, and the sidebar list — invariant 73.
    const resyncSessions = () => {
      loadedRef.current.clear();
      tracesLoadedRef.current.clear();
      eventsRef.current.onStatus(null);
      invalidate(SESSION_LISTS);
      const sid = eventsRef.current.activeSession();
      if (!sid || deletedRef.current.has(sid)) return;
      loadTimeline(sid).catch(() => toast.error('Could not refresh the session — reopen it to retry'));
      (api.sessions.tasks(sid) as Promise<TaskRow[]>)
        .then(rows => { if (rows && rows.length > 0) updateSS(sid, s => mergeTaskRows(s, rows)); })
        .catch(() => undefined);
      tracesLoadedRef.current.add(sid);
      fetchTraces(sid, true);
    };

    // A reconnect while the tab is hidden defers the resync to its next visible
    // moment; live runs are re-subscribed either way.
    let resyncPending = false;
    ws.onReconnect = () => {
      // The role may have changed while away (a 1008 close says so): refetch
      // who we are.
      window.dispatchEvent(new Event(ME_RELOAD));
      for (const runId of Object.values(sessionRunRef.current)) resubscribe(runId, undefined, false);
      if (document.visibilityState === 'hidden') {
        // The title count reads the sidebar's statuses while hidden: relist
        // now, re-read the rest when seen.
        eventsRef.current.onStatus(null);
        invalidate(SESSION_LISTS);
        resyncPending = true;
        return;
      }
      resyncPending = false;
      resyncSessions();
    };
    const onVisibilityChange = () => {
      if (document.visibilityState !== 'visible' || !resyncPending || !ws.isConnected()) return;
      resyncPending = false;
      resyncSessions();
    };
    document.addEventListener('visibilitychange', onVisibilityChange);

    ws.connect();
    // The pending-frame map lives for the hook's lifetime; the cleanup clears
    // what is queued at unmount.
    const rafPending = rafPendingRef.current;
    return () => {
      document.removeEventListener('visibilitychange', onVisibilityChange);
      ws.close();
      if (rafIdRef.current) {
        cancelAnimationFrame(rafIdRef.current);
        rafIdRef.current = 0;
      }
      rafPending.clear();
    };
  }, [updateSS, reloadMessages, scheduleFrame, tasks, loadTimeline, fetchTraces, setQueued]);

  // watchTask opens the Inspector's live view of a task: snapshot the child
  // session's transcript + traces, then the router streams the live tail in.
  // unwatchTask drops everything.
  const watchTask = useCallback((sid: string, taskId: string, childSessionId: string) => {
    tasks.watch(sid, taskId, childSessionId);
    updateSS(sid, s => ({ ...s, taskView: { taskId, childSessionId, messages: [], streaming: '', reasoning: '', traceRuns: {}, loaded: false } }));
    Promise.all([
      // fetchTimeline, not raw messages: its pending-approval merge is what
      // puts a paused task's card in the view.
      fetchTimeline(childSessionId),
      (api.sessions.traces(childSessionId, { summary: true }) as Promise<TraceRow[] | null>).catch(() => [] as TraceRow[]),
    ]).then(([{ timeline }, traceRows]) => {
      if (tasks.watching()?.taskId !== taskId) return; // switched away meanwhile
      // Grouped by run, one group per attempt, in row (= time) order, like the
      // chat's load.
      const traceRuns: Record<string, TraceEvent[]> = {};
      for (const ev of traceRows || []) {
        if (ev.kind !== 'span') continue;
        const rid = ev.run_id || 'unknown';
        if (!traceRuns[rid]) traceRuns[rid] = [];
        traceRuns[rid].push(traceEventFromRow(ev));
      }
      updateSS(sid, s => {
        if (!s.taskView || s.taskView.taskId !== taskId) return s;
        // Live spans that raced the fetch win (upsert by span id; a live-only
        // run keeps its group).
        for (const [rid, liveEvents] of Object.entries(s.taskView.traceRuns)) {
          const merged = [...(traceRuns[rid] || [])];
          for (const live of liveEvents) {
            const idx = live.span_id ? merged.findIndex(e => e.span_id === live.span_id) : -1;
            if (idx >= 0) merged[idx] = live; else merged.push(live);
          }
          traceRuns[rid] = merged;
        }
        // Snapshot wins: the child rows share the live turn's runId, so
        // mergeLiveTail would drop the in-flight turn; terminal events refetch
        // (refetchTaskView), closing the gap.
        return { ...s, taskView: { ...s.taskView, messages: timeline, traceRuns, loaded: true } };
      });
    }).catch(() => {
      updateSS(sid, s => (s.taskView && s.taskView.taskId === taskId ? { ...s, taskView: { ...s.taskView, loaded: true } } : s));
    });
  }, [updateSS, fetchTimeline, tasks]);

  const unwatchTask = useCallback((sid: string) => tasks.unwatch(sid), [tasks]);

  // forgetLoaded drops the "already fetched" mark so the next loadSession
  // re-reads (a branch switch changed the shape server-side) and bumps the
  // generation to drop in-flight fetches of the old path.
  const forgetLoaded = useCallback((sid: string) => {
    loadedRef.current.delete(sid);
    timelineGenRef.current[sid] = (timelineGenRef.current[sid] || 0) + 1;
    updateSS(sid, s => ({ ...s, loaded: false, entries: [] }));
  }, [updateSS]);

  return { wsRef, sessionRunRef, connected, loadSession, loadTasks, loadTraces, loadSpanPayload, deleteSession, forgetLoaded, watchTask, unwatchTask, queueInput, dropQueued };
}
