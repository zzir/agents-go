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

  const reloadMessages = useCallback((sid: string) => {
    api.sessions.messages(sid).then((msgs: any[]) => {
      const timeline = buildTimeline(msgs);
      updateSS(sid, s => ({ ...s, messages: timeline }));
    });
  }, [updateSS]);

  const loadSession = useCallback((sid: string): Promise<void> => {
    if (!sid || loadedRef.current.has(sid)) return Promise.resolve();
    loadedRef.current.add(sid);
    const msgP = api.sessions.messages(sid).then((msgs: any[]) => {
      const timeline = buildTimeline(msgs);
      updateSS(sid, s => s.loaded ? s : { ...s, messages: timeline, loaded: true });
    });
    // A run paused for approval isn't persisted to messages (the SDK saves on
    // completion), so rebuild any pending tool-call cards from the durable
    // approvals so the user can act on them after a reload/restart.
    api.sessions.approvals(sid).then((pending: Array<{ run_id: string; tool_calls?: Array<{ tool_call_id: string; tool_name: string; arguments: string }> }>) => {
      if (!pending || pending.length === 0) return;
      const toolCalls = pending.flatMap(p => (p.tool_calls || []).map(tc => ({
        tool_call_id: tc.tool_call_id, tool_name: tc.tool_name, arguments: tc.arguments,
        output: null as string | null, status: null as string | null, needs_approval: true,
      })));
      if (toolCalls.length === 0) return;
      const runId = pending[0].run_id;
      updateSS(sid, s => {
        // Don't duplicate a card the live stream already rendered.
        const seen = new Set<string>();
        for (const m of s.messages) {
          if (m.role !== 'turn') continue;
          for (const part of (m as { parts?: Array<{ type: string; toolCalls?: Array<{ tool_call_id: string }> }> }).parts || []) {
            if (part.type === 'tools') for (const tc of part.toolCalls || []) seen.add(tc.tool_call_id);
          }
        }
        const fresh = toolCalls.filter(tc => !seen.has(tc.tool_call_id));
        if (fresh.length === 0) return s;
        const turn = { role: 'turn' as const, parts: [{ type: 'tools' as const, toolCalls: fresh }], runId, messageId: Number.MAX_SAFE_INTEGER };
        return { ...s, messages: [...s.messages, turn] };
      });
    }).catch(() => {});
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
  }, [updateSS]);

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
      updateSS(sid, s => ({
        ...s, running: true, compacting: false, liveRunId: p.run_id, liveStartedAt: Date.now(), liveAgentName: null, loaded: true,
        messages: [...s.messages, { role: 'turn', parts: [], runId: p.run_id }],
        traceRuns: { ...s.traceRuns, [p.run_id]: [] },
      }));
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
          if (text) parts.push({ type: 'text', content: text });
          msgs[msgs.length - 1] = { ...last, parts };
        }
        return { ...s, messages: msgs, streaming: '', reasoning: '', running: false, compacting: false, liveRunId: null, liveStartedAt: null, liveAgentName: null };
      });
      reloadMessages(sid);
    });

    ws.on('run.error', (p: { run_id?: string; session_id?: string; code?: string; message: string }) => {
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
      updateSS(sid, s => {
        const msgs = [...s.messages] as Array<{ role: string; content?: string; parts?: Array<{ type: string; content?: string }> }>;
        const last = msgs[msgs.length - 1];
        if (last?.role === 'turn') {
          const parts = [...(last.parts || [])];
          if (thinking) parts.push({ type: 'thinking', content: thinking });
          if (remaining) parts.push({ type: 'text', content: remaining });
          parts.push({ type: 'error', content: p.message });
          msgs[msgs.length - 1] = { ...last, parts };
        } else {
          msgs.push({ role: 'turn', parts: [{ type: 'error', content: p.message }] });
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

    ws.on('run.handoff', (p: { run_id: string; from: string; to: string }) => {
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      updateSS(sid, s => ({
        ...s, messages: [...s.messages, { role: 'system', content: 'Handoff: ' + p.from + ' → ' + p.to }],
      }));
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
