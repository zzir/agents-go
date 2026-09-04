import type { AttachmentMeta } from '@/lib/attachments';
import { useCallback, useEffect, useRef, useState } from 'react';
import { WSClient } from '@/lib/ws';
import { EV, ERR, type RunDiagnostic, type TaskRow } from '@/lib/protocol';
import { buildTimeline, type DisplayExtra, type EntryView } from '@/lib/timeline';
import {
  ensureLiveTurn, mergeLiveTail, appendMessageItem, appendReasoningItem, finalizeTurn,
  appendErrorPart, appendCancelledPart, appendToolCall, applyToolResult, syncTaskCard, appendToolProgress, appendHandoffPart,
  TERMINAL_TASK_STATUSES,
} from '@/lib/streamReducer';
import { api, clearToken } from '@/lib/api';
import { resyncAfterGap, type GapResync } from '@/lib/gapResync';
import { toast } from '@/lib/toast';
import { ME_RELOAD } from '@/lib/me';
import {
  createTaskRouter, mergeTaskRows, seedTaskRows, withPendingTaskApprovals,
  type TaskState, type TaskViewState, type TaskRouter,
} from '@/lib/taskEvents';

import type { TraceEventData as TraceEvent } from '@/features/chat/TracePanel';

export { taskStateFromRow, taskRetryable } from '@/lib/taskEvents';
export type { TaskState, TaskViewState } from '@/lib/taskEvents';

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
  // Every persisted run's question and whether it is on the active branch,
  // fetched with the traces (loadTraces): what labels a trace card whose
  // exchange lies outside the page of history loaded — the timeline pages,
  // the traces do not.
  runQuestions: Record<string, { question: string; onPath: boolean }>;
  liveRunId: string | null;
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
  // tasksLoaded is set once the durable task rows have been asked for — what
  // tells a task deep link "not here yet" from "not here".
  tasksLoaded: boolean;
  // Set when that fetch failed, so an empty task list reads as "could not load"
  // rather than "no tasks"; cleared by a successful load.
  tasksError?: string;
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

export type UpdateSSFn = (sid: string, updater: (s: SessionState) => SessionState) => void;

export function defaultSS(): SessionState {
  return {
    messages: [], streaming: '', reasoning: '', running: false, compacting: false, diagnostics: [],
    traceRuns: {}, runQuestions: {}, liveRunId: null, loaded: false,
    entries: [], hasMore: false, loadingMore: false, tasks: {}, tasksLoaded: false, taskView: null,
  };
}

// TraceRow is a stored span as GET /sessions/:id/traces returns it.
export interface TraceRow {
  run_id?: string; parent_run_id?: string; kind?: string; name?: string; data?: string; detail?: string;
  error?: string; span_id?: string; parent_id?: string; started_at?: string; ended_at?: string; payload_omitted?: boolean;
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
    data: Object.keys(parsed).length > 0 ? parsed : null,
    duration: spanDuration(ev.started_at, ev.ended_at), payloadOmitted: !!ev.payload_omitted,
  };
}

// A live trace.span event in the panel's shape.
interface TraceSpanEvent {
  run_id: string; parent_run_id?: string; name: string; type?: string; span_id?: string; parent_id?: string;
  error?: string; started_at?: string; ended_at?: string; data?: Record<string, unknown>; payload_omitted?: boolean;
}

function traceEventFromLive(p: TraceSpanEvent): TraceEvent {
  return {
    kind: 'span', name: p.name, type: p.type || '',
    span_id: p.span_id, parent_id: p.parent_id, parent_run_id: p.parent_run_id,
    error: p.error, started_at: p.started_at, ended_at: p.ended_at,
    data: p.data || null, duration: spanDuration(p.started_at, p.ended_at), payloadOmitted: !!p.payload_omitted,
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
  const next = { ...cur, data: full.data ?? cur.data, payloadOmitted: false };
  return { ...runs, [runId]: [...events.slice(0, idx), next, ...events.slice(idx + 1)] };
}

export function useAgentSocket(updateSSRaw: UpdateSSFn) {
  const wsRef = useRef<WSClient | null>(null);
  // Optimistic true: the indicator marks a LOST connection, not a pending one.
  const [connected, setConnected] = useState(true);
  // Conversations deleted in this page (deleteSession): a late event of the
  // delete cascade's own, or a fetch that was in flight when the conversation
  // went, must not rebuild one — every write this hook makes goes through here.
  const deletedRef = useRef<Set<string>>(new Set());
  const updateSS = useCallback<UpdateSSFn>((sid, fn) => {
    if (deletedRef.current.has(sid)) return;
    updateSSRaw(sid, fn);
  }, [updateSSRaw]);
  const runMapRef = useRef<Record<string, string>>({});
  const sessionRunRef = useRef<Record<string, string>>({});
  const streamBufsRef = useRef<Record<string, string>>({});
  const reasoningBufsRef = useRef<Record<string, string>>({});
  // Per-run set of completed message/reasoning item ids already folded into the
  // timeline. Hub replays (reconnect) re-deliver those events; deduping by item
  // id — rather than by text — keeps a genuinely repeated identical message from
  // being dropped as if it were a replay.
  const appendedItemsRef = useRef<Record<string, Set<string>>>({});
  // Each run's last resync after a gap (see the run.gap handler): when, and
  // from which cursor, so a range the ring has evicted is asked for once.
  const gapResyncRef = useRef<Record<string, GapResync>>({});
  const loadedRef = useRef<Set<string>>(new Set());
  // Sessions whose persisted traces have been pulled (see loadTraces).
  const tracesLoadedRef = useRef<Set<string>>(new Set());
  // Per-session timeline generation, bumped by forgetLoaded (a branch move):
  // a fetch launched before the bump describes a path the session is no
  // longer on, and its late resolution is dropped.
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
  // the stored entries, with a durable pending approval's tool calls merged
  // INTO the persisted turn they belong to (the prompt and the safe prefix
  // are stored before the pause), fetched together and applied as one update.
  const fetchTimeline = useCallback(async (sid: string, limit = HISTORY_PAGE): Promise<TimelinePage> => {
    type PendingApproval = { run_id: string; user_input?: string; task_id?: string; tool_calls?: Array<{ tool_call_id: string; tool_name: string; arguments: string }> };
    const [msgs, pendingAll] = await Promise.all([
      api.sessions.messages(sid, { limit }) as Promise<EntryView[]>,
      (api.sessions.approvals(sid) as Promise<PendingApproval[]>).catch(() => [] as PendingApproval[]),
    ]);
    // A background task's approval surfaces on its chip, never in the chat
    // timeline (its call ids belong to the task's hidden transcript).
    const taskPending = (pendingAll || []).filter(p => p.task_id);
    if (taskPending.length > 0) updateSS(sid, s => withPendingTaskApprovals(s, taskPending));
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
    // The synthesized rows have no row id: a pending approval was never
    // persisted, so there is nothing to fork from or anchor to.
    const out = [...timeline];

    // The timeline may already hold this run's user bubble and turn: merge
    // the pending tool calls into that turn rather than appending duplicates.
    // Only when nothing for this run is persisted is the paused turn
    // reconstructed whole.
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
    if (userInput && !hasUser) out.push({ role: 'user', content: userInput, runId, messageId: undefined });
    out.push({ role: 'turn', parts: [{ type: 'tools', toolCalls }], runId, messageId: undefined });
    return { ...page, timeline: out };
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
    }).catch((e: { status?: number }) => {
      // The persisted timeline did not reload behind the optimistic stream.
      // A conversation gone (deleted here or elsewhere: 404) has nothing to refresh.
      if (deletedRef.current.has(sid) || e?.status === 404) return;
      toast.error('Could not refresh the conversation — reopen it to retry');
    });
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
    (api.sessions.tasks(sid) as Promise<TaskRow[]>)
      .then(rows => {
        if (!rows || rows.length === 0) {
          updateSS(sid, s => s.tasksLoaded && !s.tasksError ? s : { ...s, tasksLoaded: true, tasksError: undefined });
          return;
        }
        updateSS(sid, s => ({ ...seedTaskRows(s, rows), tasksError: undefined }));
      }).catch((e: { status?: number }) => {
        if (deletedRef.current.has(sid) || e?.status === 404) return;
        // Loaded-with-error, not loaded-empty: the panel must not read it as "no tasks".
        updateSS(sid, s => ({ ...s, tasksLoaded: true, tasksError: 'The task list could not be loaded' }));
        toast.error('Could not load background tasks — reopen the conversation to retry');
      });
    return msgP;
  }, [fetchTimeline, updateSS]);

  // loadTraces pulls the session's persisted span SUMMARY (payloads stay lazy,
  // fetched per span by loadSpanPayload), once per session, on session load:
  // the chat labels each turn with its run span's duration, so the data can't
  // wait for a lens to open. Live runs stream their spans over the WS
  // regardless; this backfills history. The runs' questions come with them: the
  // traces cover the whole session while the timeline is paged, so a card's
  // exchange may not be on screen to label it from.
  const loadTraces = useCallback((sid: string) => {
    if (!sid || tracesLoadedRef.current.has(sid)) return;
    tracesLoadedRef.current.add(sid);
    (api.sessions.runs(sid) as Promise<Array<{ run_id: string; question: string; on_path: boolean }> | null>)
      .then(rows => {
        if (!rows || rows.length === 0) return;
        const runQuestions: SessionState['runQuestions'] = {};
        for (const r of rows) runQuestions[r.run_id] = { question: r.question || '', onPath: r.on_path !== false };
        updateSS(sid, s => ({ ...s, runQuestions }));
      })
      .catch(() => undefined); // labels degrade to run ids; the spans still load
    // The SUMMARY listing: every span's row without its payload — the model
    // request and reply, a tool's arguments and result are nearly all of a
    // session's trace bytes, and parsing them on open is what stalls the
    // page. A row opens its payload on demand (loadSpanPayload).
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
      // Merge per run id with live data winning: a run.started that landed
      // during this fetch has already seeded (and keeps updating) its own
      // run's entry.
      updateSS(sid, s => ({ ...s, traceRuns: { ...runs, ...s.traceRuns } }));
    }).catch(() => {
      // Roll back the mark so the next lens open retries instead of leaving
      // the panel empty for good.
      tracesLoadedRef.current.delete(sid);
    });
  }, [updateSS]);

  // loadSpanPayload fetches one span whole — what the summary listing (or the
  // live cap) left out — and folds it into the span wherever the panel holds
  // it: the chat's trace groups, or the inspected task's (spanSessionId is
  // the session whose stored rows those are — the chat's own, or the task's
  // child). Rejects when the row is not there (a live span not yet ended).
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

  // deleteSession forgets a conversation the server has deleted: its load
  // marks, and every run the routing tables still map to it — the cascade's
  // own terminal events (the run it stopped, the tasks it cancelled) arrive
  // after the delete and would otherwise rebuild the session's state from
  // nothing, one ghost per deleted conversation for the life of the page.
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
    }
    delete sessionRunRef.current[deletedId];
    tasks.forgetSession(deletedId);
  }, [tasks]);

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

    ws.onStatus = setConnected;

    // dropRunRefs clears a terminal run's routing bookkeeping — any chat-path
    // refs a run acquired before its background identity was known (safe
    // no-ops otherwise).
    const dropRunRefs = (runId: string) => {
      const sid = runMapRef.current[runId];
      delete streamBufsRef.current[runId];
      delete reasoningBufsRef.current[runId];
      delete appendedItemsRef.current[runId];
      delete gapResyncRef.current[runId];
      delete runMapRef.current[runId];
      if (sid && sessionRunRef.current[sid] === runId) delete sessionRunRef.current[sid];
    };

    ws.on(EV.runStarted, (p: { session_id?: string; run_id: string; input?: string; attachments?: AttachmentMeta[]; parent_session_id?: string; parent_run_id?: string; task_id?: string; kind?: string; tool_call_id?: string; label?: string; attempt?: number; max_attempts?: number }) => {
      // A background task run — a sub-agent's or a workflow step's — is the
      // router's, and stays out of every chat-timeline path.
      if (tasks.runStarted(p)) return;
      const sid = p.session_id;
      if (!sid || deletedRef.current.has(sid)) return;
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
        const appended = ensureLiveTurn(s.messages, p.run_id, p.input, p.attachments);
        return {
          ...s, running: true, compacting: false, diagnostics: [], liveRunId: p.run_id,
          loaded: true,
          messages: appended || s.messages,
          traceRuns: { ...s.traceRuns, [p.run_id]: s.traceRuns[p.run_id] || [] },
        };
      });
    });

    ws.on(EV.taskUpdated, (p: TaskRow) => tasks.taskUpdated(p));

    ws.on(EV.runStep, (p: { run_id: string; delta: string }) => {
      if (tasks.step(p)) return;
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      streamBufsRef.current[p.run_id] = (streamBufsRef.current[p.run_id] || '') + p.delta;
      scheduleFrame('step:' + p.run_id, () => {
        const buf = streamBufsRef.current[p.run_id];
        if (buf !== undefined) updateSS(sid, s => ({ ...s, streaming: buf }));
      });
    });

    ws.on(EV.runReasoning, (p: { run_id: string; delta: string }) => {
      if (tasks.reasoning(p)) return;
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
      if (tasks.message(p)) return;
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
      if (tasks.reasoningItem(p)) return;
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
      if (tasks.output(p)) { dropRunRefs(p.run_id); return; }
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      const text = p.final_output || streamBufsRef.current[p.run_id] || '';
      const thinking = reasoningBufsRef.current[p.run_id] || '';
      delete streamBufsRef.current[p.run_id];
      delete reasoningBufsRef.current[p.run_id];
      delete appendedItemsRef.current[p.run_id];
      delete gapResyncRef.current[p.run_id];
      delete runMapRef.current[p.run_id];
      delete sessionRunRef.current[sid];
      updateSS(sid, s => {
        const msgs = finalizeTurn(s.messages, text, thinking);
        return { ...s, messages: msgs || s.messages, streaming: '', reasoning: '', running: false, compacting: false, liveRunId: null };
      });
      reloadMessages(sid);
    });

    ws.on(EV.runError, (p: { run_id?: string; session_id?: string; code?: string; message: string; guardrail?: string; stage?: string }) => {
      if (p.run_id && tasks.error({ run_id: p.run_id, message: p.message })) { dropRunRefs(p.run_id); return; }
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
            const msgs = s.messages as Array<{ role?: string; clientMsgId?: string; messageId?: string; runId?: string }>;
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
          delete gapResyncRef.current[p.run_id];
        }
        if (staleSid) {
          delete sessionRunRef.current[staleSid];
          updateSS(staleSid, s => ({ ...s, streaming: '', reasoning: '', running: false, compacting: false, liveRunId: null }));
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
      delete gapResyncRef.current[rid];
      delete runMapRef.current[rid];
      delete sessionRunRef.current[sid];
      // A guardrail block carries the guardrail name + stage so the turn renders
      // a distinct "blocked" card instead of a generic error.
      const errPart = p.code === ERR.guardrailTripwire
        ? { type: 'error' as const, content: p.message, guardrail: p.guardrail, stage: p.stage }
        : { type: 'error' as const, content: p.message };
      updateSS(sid, s => ({
        ...s, messages: appendErrorPart(s.messages, errPart, thinking, remaining),
        streaming: '', reasoning: '', running: false, compacting: false, liveRunId: null,
      }));
      // A guardrail block already rendered its typed card (with the retracted
      // answer above it) optimistically. A reload would replace that with the
      // persisted timeline, which — since the SDK never persists a tripped output
      // (Python parity) — drops the answer; the card itself survives via the
      // persisted guardrail/stage. Keep the richer optimistic view for this session.
      if (p.code !== ERR.guardrailTripwire) reloadMessages(sid);
    });

    ws.on(EV.runCancelled, (p: { run_id?: string; code?: string }) => {
      if (p?.run_id && tasks.cancelled({ run_id: p.run_id })) { dropRunRefs(p.run_id); return; }
      const rid = p?.run_id;
      const sid = rid ? runMapRef.current[rid] : null;
      if (!sid || !rid) return;
      const remaining = streamBufsRef.current[rid] || '';
      const thinking = reasoningBufsRef.current[rid] || '';
      delete streamBufsRef.current[rid];
      delete reasoningBufsRef.current[rid];
      delete appendedItemsRef.current[rid];
      delete gapResyncRef.current[rid];
      delete runMapRef.current[rid];
      delete sessionRunRef.current[sid];
      // The marker shows immediately, mirroring how run.error appends its card
      // optimistically, instead of waiting on the async reload (which the next
      // run's start can also skip).
      updateSS(sid, s => {
        const msgs = appendCancelledPart(s.messages, thinking, remaining);
        return { ...s, messages: msgs || s.messages, streaming: '', reasoning: '', running: false, compacting: false, liveRunId: null };
      });
      reloadMessages(sid);
    });

    ws.on(EV.runToolCall, (p: { run_id: string; tool_call_id: string; tool_name: string; arguments: string; needs_approval?: boolean }) => {
      if (tasks.toolCall(p)) return;
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      // A hub replay (reconnect, a gap's resync) delivers the call again: it
      // patches the card it already made, and must neither flush the streamed
      // text into a part a second time nor blank the in-flight preview.
      const seen = appendedItemsRef.current[p.run_id] || (appendedItemsRef.current[p.run_id] = new Set());
      const replayed = seen.has('tc:' + p.tool_call_id);
      seen.add('tc:' + p.tool_call_id);
      const flushed = replayed ? '' : (streamBufsRef.current[p.run_id] || '');
      if (!replayed) streamBufsRef.current[p.run_id] = '';
      // Normalize needs_approval: the wire always carries the bool, but a
      // replayed timeline only ever marks pending calls — carrying an explicit
      // false would make the streamed turn differ from its reload (the
      // isomorphism test pins this).
      const tc = { tool_call_id: p.tool_call_id, tool_name: p.tool_name, arguments: p.arguments, needs_approval: p.needs_approval || undefined, status: null as string | null, output: null as string | null };
      updateSS(sid, s => {
        let msgs = appendToolCall(s.messages, tc, flushed);
        // The spawned task may already be terminal: parent and task runs are
        // delivered on independent subscriptions with no cross-run ordering,
        // so the task's terminal fold can run before this card exists. The
        // outcome is still in s.tasks — fold it onto the card (invariant 21).
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

    // Live output from a tool that is still running. It is not the answer —
    // run.tool_result is — so it accumulates on the card and is replaced when
    // the result lands.
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

    // The run paused for tool approval: nothing is executing until the user
    // decides, so live indicators come down and `running` reflects the truth
    // (reloads merge the paused turn from the durable approvals meanwhile).
    // The mappings stay: the approval decision RESUMES THE SAME run id, so a
    // later run.started on this id flips the session back to live seamlessly.
    ws.on(EV.runInterrupted, (p: { run_id: string }) => {
      if (tasks.interrupted(p)) return;
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      delete streamBufsRef.current[p.run_id];
      delete reasoningBufsRef.current[p.run_id];
      updateSS(sid, s => ({
        ...s, streaming: '', reasoning: '', running: false, compacting: false,
        liveRunId: null,
      }));
    });

    // This connection fell behind and the server dropped events for it — for
    // this connection only; the run is unaffected. The repair is the hub's own
    // replay ring: re-subscribing from the last good sequence number delivers
    // the dropped events again (items dedup by id). Chat runs only — the
    // inspector's live view of a task run keeps no item ids to dedup a replay
    // by. A range the ring has already evicted is not asked for
    // (resyncAfterGap) — see invariant 14.
    ws.on(EV.runGap, (p: { run_id: string; dropped: number; last_good: number }) => {
      if (!runMapRef.current[p.run_id]) return;
      const now = Date.now();
      if (!resyncAfterGap(gapResyncRef.current[p.run_id], p.last_good, now)) return;
      gapResyncRef.current[p.run_id] = { at: now, cursor: p.last_good };
      console.warn(`dropped ${p.dropped} event(s) after seq ${p.last_good}; resyncing from the hub's replay`);
      ws.send(EV.runSubscribe, { run_id: p.run_id, from_seq: p.last_good });
    });

    // Trouble the run survived. It arrives with the terminal event, so it is
    // recorded rather than animated: the point is the record afterwards.
    ws.on(EV.runDiagnostic, (p: RunDiagnostic & { run_id: string }) => {
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      // A hub replay (reconnect, a gap's resync) delivers it again: the same
      // record twice is one record.
      updateSS(sid, s => s.diagnostics.some(d => d.type === p.type && d.code === p.code && d.message === p.message)
        ? s
        : { ...s, diagnostics: [...s.diagnostics, p] });
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
    ws.on(EV.runHandoff, (p: { run_id: string; from: string; to?: string; from_id?: string; to_id?: string }) => {
      const handoff = { from: p.from, to: p.to || '', fromId: p.from_id, toId: p.to_id };
      if (tasks.handoff(p, handoff)) return;
      const sid = runMapRef.current[p.run_id];
      if (!sid || !p.to) return;
      // Hub replays (reconnect) re-deliver run.handoff; the reducer dedups like
      // run.message / run.reasoning_item so a reconnect mid-run doesn't stack rows.
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

    // On reconnect, any run that kept executing server-side may have advanced
    // or finished while we were away. Re-subscribe to still-live runs (the
    // hub replays buffered events) and reload persisted history so a run that
    // completed offline shows its result.
    ws.onReconnect = () => {
      // The role may have changed while away (a 1008 close is how the server
      // says so): the app refetches who we are.
      window.dispatchEvent(new Event(ME_RELOAD));
      // Task runs are not resubscribed (their terminal events may be gone from
      // the hub entirely) — re-pull the durable rows so statuses that changed
      // during the outage land in the chips.
      for (const sid of loadedRef.current) {
        (api.sessions.tasks(sid) as Promise<TaskRow[]>)
          .then(rows => { if (rows && rows.length > 0) updateSS(sid, s => mergeTaskRows(s, rows)); })
          .catch(() => undefined);
      }
      for (const [sid, runId] of Object.entries(sessionRunRef.current)) {
        ws.send(EV.runSubscribe, { run_id: runId });
        reloadMessages(sid);
      }
    };

    ws.connect();
    // The pending-frame map is one Map for the hook's lifetime; the cleanup
    // clears whatever is queued at unmount.
    const rafPending = rafPendingRef.current;
    return () => {
      ws.close();
      if (rafIdRef.current) {
        cancelAnimationFrame(rafIdRef.current);
        rafIdRef.current = 0;
      }
      rafPending.clear();
    };
  }, [updateSS, reloadMessages, scheduleFrame, tasks]);

  // watchTask opens the Inspector's live view of a task: snapshot the child
  // session's persisted transcript + traces, then let the router stream the
  // live tail into it. unwatchTask drops everything.
  const watchTask = useCallback((sid: string, taskId: string, childSessionId: string) => {
    tasks.watch(sid, taskId, childSessionId);
    updateSS(sid, s => ({ ...s, taskView: { taskId, childSessionId, messages: [], streaming: '', reasoning: '', traceRuns: {}, loaded: false } }));
    Promise.all([
      // fetchTimeline (not raw messages): a task paused on an approval keeps
      // its dangling tool call out of messages (persist boundary) — the
      // pending-approval merge is what puts the approval card in the view.
      fetchTimeline(childSessionId),
      (api.sessions.traces(childSessionId, { summary: true }) as Promise<TraceRow[] | null>).catch(() => [] as TraceRow[]),
    ]).then(([{ timeline }, traceRows]) => {
      if (tasks.watching()?.taskId !== taskId) return; // switched away meanwhile
      // Grouped by run — one group per attempt, row order (= time order)
      // deciding group order, exactly like the chat drawer's load.
      const traceRuns: Record<string, TraceEvent[]> = {};
      for (const ev of traceRows || []) {
        if (ev.kind !== 'span') continue;
        const rid = ev.run_id || 'unknown';
        if (!traceRuns[rid]) traceRuns[rid] = [];
        traceRuns[rid].push(traceEventFromRow(ev));
      }
      updateSS(sid, s => {
        if (!s.taskView || s.taskView.taskId !== taskId) return s;
        // Live spans that raced the fetch win (upsert by span id, per run —
        // a live-only run keeps its whole group).
        for (const [rid, liveEvents] of Object.entries(s.taskView.traceRuns)) {
          const merged = [...(traceRuns[rid] || [])];
          for (const live of liveEvents) {
            const idx = live.span_id ? merged.findIndex(e => e.span_id === live.span_id) : -1;
            if (idx >= 0) merged[idx] = live; else merged.push(live);
          }
          traceRuns[rid] = merged;
        }
        // Snapshot wins: the child rows share the live turn's runId, so a
        // mergeLiveTail would drop the in-flight turn wholesale. Terminal
        // events refetch (the router's refetchTaskView), which closes the gap
        // for good.
        return { ...s, taskView: { ...s.taskView, messages: timeline, traceRuns, loaded: true } };
      });
    }).catch(() => {
      updateSS(sid, s => (s.taskView && s.taskView.taskId === taskId ? { ...s, taskView: { ...s.taskView, loaded: true } } : s));
    });
  }, [updateSS, fetchTimeline, tasks]);

  const unwatchTask = useCallback((sid: string) => tasks.unwatch(sid), [tasks]);

  // loadEarlier prepends the previous page of history. It rebuilds the
  // timeline from ALL fetched entries rather than prepending the new page's
  // timeline to the old one: buildTimeline folds turns across entries, and
  // assembling the halves separately splits whichever turn straddles the
  // page boundary. beforeId is passed IN, not read from state: a state
  // updater runs when React processes the update, not at the call.
  const loadEarlier = useCallback((sid: string, beforeId: string): void => {
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
          // merge is scoped to the CURRENT live run.
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
      .catch(() => {
        updateSS(sid, s => ({ ...s, loadingMore: false }));
        if (!deletedRef.current.has(sid)) toast.error('Could not load earlier messages');
      })
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

  return { wsRef, sessionRunRef, connected, loadSession, loadTraces, loadSpanPayload, deleteSession, loadEarlier, forgetLoaded, watchTask, unwatchTask };
}
