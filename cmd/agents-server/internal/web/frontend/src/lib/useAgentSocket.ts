import { useCallback, useEffect, useRef } from 'react';
import { WSClient } from '@/lib/ws';
import { buildTimeline, patchToolCall } from '@/lib/timeline';
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
  // messages from the DB merged with the paused turn rebuilt from any durable
  // pending approval (the SDK persists a turn to `messages` only on completion,
  // so during a pause the approval row is the only holder of the user's prompt
  // and the pending tool calls). Fetching both together and applying the result
  // as one state update removes the messages/approvals race that made the
  // approval card appear or vanish depending on which response landed last.
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
    // The paused turn is always the latest: user bubble (held only by the
    // approval row until the turn completes), then its tool-call card.
    const runId = pending[0].run_id;
    const userInput = pending[0].user_input || '';
    const out = [...timeline];
    if (userInput) out.push({ role: 'user', content: userInput, runId, messageId: Number.MAX_SAFE_INTEGER - 1 });
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
      // Live events may have landed while fetching (loaded flipped true) —
      // they are fresher than this snapshot, so keep them.
      updateSS(sid, s => s.loaded ? s : { ...s, messages: timeline, loaded: true });
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
      updateSS(sid, s => Object.keys(s.traceRuns).length > 0 ? s : { ...s, traceRuns: runs });
    }).catch(() => {});
    return msgP;
  }, [fetchTimeline, updateSS]);

  const deleteSession = useCallback((deletedId: string) => {
    loadedRef.current.delete(deletedId);
  }, []);

  useEffect(() => {
    const ws = new WSClient();
    wsRef.current = ws;

    ws.on('run.started', (p: { session_id?: string; run_id: string }) => {
      const sid = p.session_id;
      if (!sid) return;
      runMapRef.current[p.run_id] = sid;
      sessionRunRef.current[sid] = p.run_id;
      streamBufsRef.current[p.run_id] = '';
      reasoningBufsRef.current[p.run_id] = '';
      if (!loadedRef.current.has(sid)) loadedRef.current.add(sid);
      updateSS(sid, s => {
        // Hub replays (reconnect / re-subscribe) re-deliver run.started; don't
        // grow a second live turn for a run we already track.
        const hasTurn = s.messages.some(m => m.role === 'turn' && (m as { runId?: string }).runId === p.run_id);
        return {
          ...s, running: true, compacting: false, liveRunId: p.run_id,
          liveStartedAt: hasTurn ? (s.liveStartedAt ?? Date.now()) : Date.now(),
          liveAgentName: null, loaded: true,
          messages: hasTurn ? s.messages : [...s.messages, { role: 'turn', parts: [], runId: p.run_id }],
          traceRuns: { ...s.traceRuns, [p.run_id]: s.traceRuns[p.run_id] || [] },
        };
      });
    });

    ws.on('run.step', (p: { run_id: string; delta: string }) => {
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      streamBufsRef.current[p.run_id] = (streamBufsRef.current[p.run_id] || '') + p.delta;
      scheduleFrame('step:' + p.run_id, () => {
        const buf = streamBufsRef.current[p.run_id];
        if (buf !== undefined) updateSS(sid, s => ({ ...s, streaming: buf }));
      });
    });

    ws.on('run.reasoning', (p: { run_id: string; delta: string }) => {
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
    ws.on('run.message', (p: { run_id: string; text: string }) => {
      const sid = runMapRef.current[p.run_id];
      if (!sid || !p.text) return;
      streamBufsRef.current[p.run_id] = '';
      updateSS(sid, s => {
        const msgs = [...s.messages] as Array<{ role: string; parts?: Array<{ type: string; content?: string }> }>;
        const last = msgs[msgs.length - 1];
        if (last?.role !== 'turn') return { ...s, streaming: '' };
        const parts = [...(last.parts || [])];
        // Hub replays (reconnect / re-subscribe) re-deliver run.message for
        // turns already in the timeline: appending again would duplicate.
        if (parts.some(pt => pt.type === 'text' && pt.content === p.text)) {
          return { ...s, streaming: '' };
        }
        parts.push({ type: 'text', content: p.text });
        msgs[msgs.length - 1] = { ...last, parts };
        return { ...s, messages: msgs, streaming: '' };
      });
    });

    // One completed reasoning block — a turn's full thinking text,
    // authoritative over the run.reasoning deltas that previewed it. Freezing
    // it as a thinking part (and resetting the delta buffer) scopes the live
    // "Thinking…" preview to the current turn and is the only thinking signal
    // on backends that stream no reasoning deltas.
    ws.on('run.reasoning_item', (p: { run_id: string; text: string }) => {
      const sid = runMapRef.current[p.run_id];
      if (!sid || !p.text) return;
      reasoningBufsRef.current[p.run_id] = '';
      updateSS(sid, s => {
        const msgs = [...s.messages] as Array<{ role: string; parts?: Array<{ type: string; content?: string }> }>;
        const last = msgs[msgs.length - 1];
        if (last?.role !== 'turn') return { ...s, reasoning: '' };
        const parts = [...(last.parts || [])];
        // Hub replays re-deliver run.reasoning_item for turns already in the
        // timeline: appending again would duplicate.
        if (parts.some(pt => pt.type === 'thinking' && pt.content === p.text)) {
          return { ...s, reasoning: '' };
        }
        parts.push({ type: 'thinking', content: p.text });
        msgs[msgs.length - 1] = { ...last, parts };
        return { ...s, messages: msgs, reasoning: '' };
      });
    });

    ws.on('run.output', (p: { run_id: string; final_output?: string }) => {
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      const text = p.final_output || streamBufsRef.current[p.run_id] || '';
      const thinking = reasoningBufsRef.current[p.run_id] || '';
      delete streamBufsRef.current[p.run_id];
      delete reasoningBufsRef.current[p.run_id];
      delete sessionRunRef.current[sid];
      updateSS(sid, s => {
        const msgs = [...s.messages] as Array<{ role: string; parts?: Array<{ type: string; content?: string }> }>;
        const last = msgs[msgs.length - 1];
        if (last?.role === 'turn' && (text || thinking)) {
          const parts = [...(last.parts || [])];
          if (thinking) parts.push({ type: 'thinking', content: thinking });
          // run.message already appended the final turn's text; only add it
          // here when that event did not (older server, no-item edge cases).
          if (text && !parts.some(pt => pt.type === 'text' && pt.content === text)) {
            parts.push({ type: 'text', content: text });
          }
          msgs[msgs.length - 1] = { ...last, parts };
        }
        return { ...s, messages: msgs, streaming: '', reasoning: '', running: false, compacting: false, liveRunId: null, liveStartedAt: null, liveAgentName: null };
      });
      reloadMessages(sid);
    });

    ws.on('run.error', (p: { run_id?: string; session_id?: string; code?: string; message: string; guardrail?: string; stage?: string }) => {
      // The session already has a live run (e.g. double-send from another
      // tab): the run this error names is still executing — a toast, not a
      // terminal error on the live turn.
      if (p.code === 'session_busy') {
        toast.error(p.message || 'Session already has an active run');
        return;
      }
      // The run we tried to resubscribe expired server-side (finished >15min
      // ago): clear the stale mapping and fall back to persisted history.
      if (p.code === 'run_not_found') {
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
      delete sessionRunRef.current[sid];
      // A guardrail block carries the guardrail name + stage so the turn renders
      // a distinct "blocked" card instead of a generic error.
      const errPart = p.code === 'guardrail_tripwire'
        ? { type: 'error', content: p.message, guardrail: p.guardrail, stage: p.stage }
        : { type: 'error', content: p.message };
      updateSS(sid, s => {
        const msgs = [...s.messages] as Array<{ role: string; content?: string; parts?: Array<{ type: string; content?: string; guardrail?: string; stage?: string }> }>;
        const last = msgs[msgs.length - 1];
        if (last?.role === 'turn') {
          const parts = [...(last.parts || [])];
          if (thinking) parts.push({ type: 'thinking', content: thinking });
          if (remaining) parts.push({ type: 'text', content: remaining });
          parts.push(errPart);
          msgs[msgs.length - 1] = { ...last, parts };
        } else {
          msgs.push({ role: 'turn', parts: [errPart] });
        }
        return { ...s, messages: msgs, streaming: '', reasoning: '', running: false, compacting: false, liveRunId: null, liveStartedAt: null, liveAgentName: null };
      });
      reloadMessages(sid);
    });

    ws.on('run.cancelled', (p: { run_id?: string }) => {
      const rid = p?.run_id;
      const sid = rid ? runMapRef.current[rid] : null;
      if (!sid || !rid) return;
      const remaining = streamBufsRef.current[rid] || '';
      const thinking = reasoningBufsRef.current[rid] || '';
      delete streamBufsRef.current[rid];
      delete reasoningBufsRef.current[rid];
      delete sessionRunRef.current[sid];
      updateSS(sid, s => {
        const msgs = [...s.messages] as Array<{ role: string; parts?: Array<{ type: string; content?: string }> }>;
        if (remaining || thinking) {
          const last = msgs[msgs.length - 1];
          if (last?.role === 'turn') {
            const parts = [...(last.parts || [])];
            if (thinking) parts.push({ type: 'thinking', content: thinking });
            if (remaining) parts.push({ type: 'text', content: remaining });
            msgs[msgs.length - 1] = { ...last, parts };
          }
        }
        return { ...s, messages: msgs, streaming: '', reasoning: '', running: false, compacting: false, liveRunId: null, liveStartedAt: null, liveAgentName: null };
      });
      reloadMessages(sid);
    });

    ws.on('run.tool_call', (p: { run_id: string; tool_call_id: string; tool_name: string; arguments: string; needs_approval?: boolean }) => {
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      const flushed = streamBufsRef.current[p.run_id] || '';
      streamBufsRef.current[p.run_id] = '';
      const tc = { tool_call_id: p.tool_call_id, tool_name: p.tool_name, arguments: p.arguments, needs_approval: p.needs_approval, status: null as string | null, output: null as string | null };
      updateSS(sid, s => {
        // Replays and the approval-rebuilt turn can already hold this call:
        // update the existing card instead of appending a duplicate (a stray
        // duplicate needs_approval card would pin the session red forever).
        const patched = patchToolCall(s.messages, p.tool_call_id, { tool_name: p.tool_name, arguments: p.arguments, needs_approval: p.needs_approval });
        if (patched) return { ...s, messages: patched, streaming: '' };
        const msgs = [...s.messages] as Array<{ role: string; parts?: Array<{ type: string; content?: string; toolCalls?: typeof tc[] }> }>;
        const last = msgs[msgs.length - 1];
        if (last?.role !== 'turn') return s;
        const parts = [...(last.parts || [])];
        if (flushed) parts.push({ type: 'text', content: flushed });
        const lastPart = parts[parts.length - 1];
        if (lastPart?.type === 'tools' && lastPart.toolCalls) {
          parts[parts.length - 1] = { ...lastPart, toolCalls: [...lastPart.toolCalls, tc] };
        } else {
          parts.push({ type: 'tools', toolCalls: [tc] });
        }
        msgs[msgs.length - 1] = { ...last, parts };
        return { ...s, messages: msgs, streaming: '' };
      });
    });

    ws.on('run.tool_result', (p: { run_id: string; tool_call_id: string; output: string }) => {
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      updateSS(sid, s => {
        const patched = patchToolCall(s.messages, p.tool_call_id, { output: p.output, status: 'completed' });
        return patched ? { ...s, messages: patched } : s;
      });
    });

    // The run paused for tool approval: nothing is executing until the user
    // decides, so live indicators come down and `running` reflects the truth
    // (reloads merge the paused turn from the durable approvals meanwhile).
    // The mappings stay: the approval decision RESUMES THE SAME run id, so a
    // later run.started on this id flips the session back to live seamlessly.
    ws.on('run.interrupted', (p: { run_id: string }) => {
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

    ws.on('run.agent_start', (p: { run_id: string; agent_name?: string }) => {
      const sid = runMapRef.current[p.run_id];
      if (!sid || !p.agent_name) return;
      updateSS(sid, s => s.liveAgentName === p.agent_name ? s : { ...s, liveAgentName: p.agent_name || null });
    });

    ws.on('run.compaction', (p: { run_id: string; phase: string }) => {
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      updateSS(sid, s => ({ ...s, compacting: p.phase === 'started' }));
    });

    // The completed agent switch becomes a part INSIDE the live turn: the turn
    // must stay the last message — every stream handler above and ChatView's
    // isLive check anchor on it, so a message appended after it would freeze
    // live rendering for the rest of the run. handoff_requested events (no
    // `to` yet) are preview noise and are skipped.
    ws.on('run.handoff', (p: { run_id: string; from: string; to?: string }) => {
      const sid = runMapRef.current[p.run_id];
      if (!sid || !p.to) return;
      updateSS(sid, s => {
        const msgs = [...s.messages] as Array<{ role: string; parts?: Array<{ type: string; content?: string }> }>;
        const last = msgs[msgs.length - 1];
        if (last?.role !== 'turn') return s;
        msgs[msgs.length - 1] = { ...last, parts: [...(last.parts || []), { type: 'handoff', content: p.from + ' → ' + p.to }] };
        return { ...s, messages: msgs };
      });
    });

    ws.on('trace.span', (p: { run_id: string; name: string; type?: string; span_id?: string; parent_id?: string; error?: string; started_at?: string; ended_at?: string; data?: Record<string, unknown> }) => {
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
        ws.send('run.subscribe', { run_id: runId });
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
