import { useCallback, useEffect, useRef } from 'react';
import { WSClient } from '@/lib/ws';
import { EV, ERR } from '@/lib/protocol';
import { buildTimeline } from '@/lib/timeline';
import {
  ensureLiveTurn, mergeLiveTail, appendMessageItem, appendReasoningItem, finalizeTurn,
  appendErrorPart, appendCancelledPart, appendToolCall, applyToolResult, appendHandoffPart,
} from '@/lib/streamReducer';
import { api } from '@/lib/api';
import { toast } from '@/lib/toast';

import type { TraceEventData as TraceEvent } from '@/features/chat/TracePanel';

export interface SessionState {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  messages: any[];
  streaming: string;
  reasoning: string;
  running: boolean;
  compacting: boolean;
  traceRuns: Record<string, TraceEvent[]>;
  liveRunId: string | null;
  liveStartedAt: number | null;
  liveAgentName: string | null;
  loaded: boolean;
}

type UpdateSSFn = (sid: string, updater: (s: SessionState) => SessionState) => void;

export function defaultSS(): SessionState {
  return {
    messages: [], streaming: '', reasoning: '', running: false, compacting: false,
    traceRuns: {}, liveRunId: null, liveStartedAt: null, liveAgentName: null, loaded: false,
  };
}

export function useAgentSocket(updateSS: UpdateSSFn) {
  const wsRef = useRef<WSClient | null>(null);
  const runMapRef = useRef<Record<string, string>>({});
  const sessionRunRef = useRef<Record<string, string>>({});
  const streamBufsRef = useRef<Record<string, string>>({});
  const reasoningBufsRef = useRef<Record<string, string>>({});
  // Per-run set of completed message/reasoning item ids already folded into the
  // timeline. Hub replays (reconnect) re-deliver those events; deduping by item
  // id — rather than by text — keeps a genuinely repeated identical message from
  // being dropped as if it were a replay.
  const appendedItemsRef = useRef<Record<string, Set<string>>>({});
  const loadedRef = useRef<Set<string>>(new Set());

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
  const fetchTimeline = useCallback(async (sid: string): Promise<SessionState['messages']> => {
    type PendingApproval = { run_id: string; user_input?: string; tool_calls?: Array<{ tool_call_id: string; tool_name: string; arguments: string }> };
    const [msgs, pending] = await Promise.all([
      api.sessions.messages(sid) as Promise<any[]>,
      (api.sessions.approvals(sid) as Promise<PendingApproval[]>).catch(() => [] as PendingApproval[]),
    ]);
    const timeline = buildTimeline(msgs) as SessionState['messages'];
    if (!pending || pending.length === 0) return timeline;
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
    if (toolCalls.length === 0) return timeline;
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
      return out;
    }
    const hasUser = out.some(m => m.role === 'user' && (m.runId === runId || (userInput && m.content === userInput)));
    if (userInput && !hasUser) out.push({ role: 'user', content: userInput, runId, messageId: Number.MAX_SAFE_INTEGER - 1 });
    out.push({ role: 'turn' as const, parts: [{ type: 'tools' as const, toolCalls }], runId, messageId: Number.MAX_SAFE_INTEGER });
    return out;
  }, []);

  const reloadMessages = useCallback((sid: string) => {
    fetchTimeline(sid).then(timeline => {
      // Never clobber a running session: mid-resume the paused turn exists
      // NEITHER in messages (saved on completion) nor in approvals (the row is
      // deleted as the resume's claim), so a reload in that window would blank
      // the conversation. Every terminal event sets running=false and reloads,
      // so skipping here loses nothing.
      updateSS(sid, s => s.running ? s : { ...s, messages: timeline });
    }).catch(() => {});
  }, [fetchTimeline, updateSS]);

  const loadSession = useCallback((sid: string): Promise<void> => {
    if (!sid || loadedRef.current.has(sid)) return Promise.resolve();
    loadedRef.current.add(sid);
    const msgP = fetchTimeline(sid).then(timeline => {
      // Live events may have landed while fetching (loaded flipped true, e.g.
      // a broadcast run.started from another browser's run) — merge them onto
      // the persisted snapshot instead of dropping either side.
      updateSS(sid, s => s.loaded
        ? { ...s, messages: mergeLiveTail(timeline, s.messages) }
        : { ...s, messages: timeline, loaded: true });
    });
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

    ws.on(EV.runStarted, (p: { session_id?: string; run_id: string; input?: string }) => {
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
          ...s, running: true, compacting: false, liveRunId: p.run_id,
          liveStartedAt: appended ? Date.now() : (s.liveStartedAt ?? Date.now()),
          liveAgentName: null, loaded: true,
          messages: appended || s.messages,
          traceRuns: { ...s.traceRuns, [p.run_id]: s.traceRuns[p.run_id] || [] },
        };
      });
    });

    ws.on(EV.runStep, (p: { run_id: string; delta: string }) => {
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      streamBufsRef.current[p.run_id] = (streamBufsRef.current[p.run_id] || '') + p.delta;
      scheduleFrame('step:' + p.run_id, () => {
        const buf = streamBufsRef.current[p.run_id];
        if (buf !== undefined) updateSS(sid, s => ({ ...s, streaming: buf }));
      });
    });

    ws.on(EV.runReasoning, (p: { run_id: string; delta: string }) => {
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
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      const text = p.final_output || streamBufsRef.current[p.run_id] || '';
      const thinking = reasoningBufsRef.current[p.run_id] || '';
      delete streamBufsRef.current[p.run_id];
      delete reasoningBufsRef.current[p.run_id];
      delete appendedItemsRef.current[p.run_id];
      delete sessionRunRef.current[sid];
      updateSS(sid, s => {
        const msgs = finalizeTurn(s.messages, text, thinking);
        return { ...s, messages: msgs || s.messages, streaming: '', reasoning: '', running: false, compacting: false, liveRunId: null, liveStartedAt: null, liveAgentName: null };
      });
      reloadMessages(sid);
    });

    ws.on(EV.runError, (p: { run_id?: string; session_id?: string; code?: string; message: string; guardrail?: string; stage?: string }) => {
      // The session already has a live run (e.g. double-send from another
      // tab): the run this error names is still executing — a toast, not a
      // terminal error on the live turn.
      if (p.code === ERR.sessionBusy) {
        toast.error(p.message || 'Session already has an active run');
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
        if (p.run_id) delete runMapRef.current[p.run_id];
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
      const rid = p?.run_id;
      const sid = rid ? runMapRef.current[rid] : null;
      if (!sid || !rid) return;
      const remaining = streamBufsRef.current[rid] || '';
      const thinking = reasoningBufsRef.current[rid] || '';
      delete streamBufsRef.current[rid];
      delete reasoningBufsRef.current[rid];
      delete appendedItemsRef.current[rid];
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
        const msgs = appendToolCall(s.messages, tc, flushed);
        return msgs ? { ...s, messages: msgs, streaming: '' } : s;
      });
    });

    ws.on(EV.runToolResult, (p: { run_id: string; tool_call_id: string; output: string }) => {
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      updateSS(sid, s => {
        const msgs = applyToolResult(s.messages, p.tool_call_id, p.output);
        return msgs ? { ...s, messages: msgs } : s;
      });
    });

    // The run paused for tool approval: nothing is executing until the user
    // decides, so live indicators come down and `running` reflects the truth
    // (reloads merge the paused turn from the durable approvals meanwhile).
    // The mappings stay: the approval decision RESUMES THE SAME run id, so a
    // later run.started on this id flips the session back to live seamlessly.
    ws.on(EV.runInterrupted, (p: { run_id: string }) => {
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

  return { wsRef, sessionRunRef, loadSession, deleteSession };
}
