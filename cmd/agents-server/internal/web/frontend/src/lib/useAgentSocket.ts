import { useCallback, useEffect, useRef } from 'react';
import { WSClient } from '@/lib/ws';
import { buildTimeline, formatHookDetail, patchToolCall } from '@/lib/timeline';
import { api } from '@/lib/api';

interface TraceEvent {
  kind: string;
  name: string;
  type?: string;
  span_id?: string;
  duration?: string;
  data?: unknown;
  agent?: string;
  tool?: string;
  from?: string;
  to?: string;
  detail?: string;
}

interface SessionState {
  messages: any[];
  streaming: string;
  running: boolean;
  traceRuns: Record<string, TraceEvent[]>;
  liveRunId: string | null;
  liveStartedAt: number | null;
  loaded: boolean;
  lastError?: string;
}

type UpdateSSFn = (sid: string, updater: (s: SessionState) => SessionState) => void;

export function defaultSS(): SessionState {
  return { messages: [], streaming: '', running: false, traceRuns: {}, liveRunId: null, liveStartedAt: null, loaded: false };
}

export function useAgentSocket(updateSS: UpdateSSFn) {
  const wsRef = useRef<WSClient | null>(null);
  const runMapRef = useRef<Record<string, string>>({});
  const sessionRunRef = useRef<Record<string, string>>({});
  const streamBufsRef = useRef<Record<string, string>>({});
  const loadedRef = useRef<Set<string>>(new Set());

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
    api.sessions.traces(sid).then((events: Array<{ run_id?: string; kind?: string; name?: string; data?: string; detail?: string; span_id?: string; started_at?: string; ended_at?: string }>) => {
      if (!events || events.length === 0) return;
      const runs: Record<string, TraceEvent[]> = {};
      for (const ev of events) {
        const rid = ev.run_id || 'unknown';
        if (!runs[rid]) runs[rid] = [];
        if (ev.kind === 'hook') {
          const d = ev.data || '';
          const agent = (d.match(/agent=(\S+)/) || [])[1] || '';
          const tool = (d.match(/tool=(\S+)/) || [])[1] || '';
          const from = (d.match(/from=(\S+)/) || [])[1] || '';
          const to = (d.match(/to=(\S+)/) || [])[1] || '';
          runs[rid].push({ kind: 'hook', name: ev.name || '', agent, tool, from, to, detail: ev.detail || '' });
        } else {
          let duration = '';
          if (ev.started_at && ev.ended_at) {
            const ms = new Date(ev.ended_at).getTime() - new Date(ev.started_at).getTime();
            duration = ms < 1000 ? ms + 'ms' : (ms / 1000).toFixed(1) + 's';
          }
          runs[rid].push({ kind: 'span', name: ev.name || '', type: ev.detail || '', span_id: ev.span_id, duration });
        }
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
      if (!loadedRef.current.has(sid)) loadedRef.current.add(sid);
      updateSS(sid, s => ({
        ...s, running: true, liveRunId: p.run_id, liveStartedAt: Date.now(), loaded: true,
        messages: [...s.messages, { role: 'turn', parts: [], runId: p.run_id }],
        traceRuns: { ...s.traceRuns, [p.run_id]: [] },
      }));
    });

    ws.on('run.step', (p: { run_id: string; delta: string }) => {
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      streamBufsRef.current[p.run_id] = (streamBufsRef.current[p.run_id] || '') + p.delta;
      updateSS(sid, s => ({ ...s, streaming: streamBufsRef.current[p.run_id] }));
    });

    ws.on('run.output', (p: { run_id: string; final_output?: string }) => {
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      const text = p.final_output || streamBufsRef.current[p.run_id] || '';
      delete streamBufsRef.current[p.run_id];
      delete sessionRunRef.current[sid];
      updateSS(sid, s => {
        const msgs = [...s.messages] as Array<{ role: string; parts?: Array<{ type: string; content?: string }> }>;
        if (text) {
          const last = msgs[msgs.length - 1];
          if (last?.role === 'turn') {
            msgs[msgs.length - 1] = { ...last, parts: [...(last.parts || []), { type: 'text', content: text }] };
          }
        }
        return { ...s, messages: msgs, streaming: '', running: false, liveRunId: null, liveStartedAt: null };
      });
      reloadMessages(sid);
    });

    ws.on('run.error', (p: { run_id: string; message: string }) => {
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      const remaining = streamBufsRef.current[p.run_id] || '';
      delete streamBufsRef.current[p.run_id];
      delete sessionRunRef.current[sid];
      updateSS(sid, s => {
        const msgs = [...s.messages] as Array<{ role: string; content?: string; parts?: Array<{ type: string; content?: string }> }>;
        const last = msgs[msgs.length - 1];
        if (last?.role === 'turn') {
          const parts = [...(last.parts || [])];
          if (remaining) parts.push({ type: 'text', content: remaining });
          msgs[msgs.length - 1] = { ...last, parts };
        } else {
          msgs.push({ role: 'system', content: 'Error: ' + p.message });
        }
        return { ...s, messages: msgs, streaming: '', running: false, liveRunId: null, liveStartedAt: null, lastError: p.message };
      });
      reloadMessages(sid);
    });

    ws.on('run.cancelled', (p: { run_id?: string }) => {
      const rid = p?.run_id;
      const sid = rid ? runMapRef.current[rid] : null;
      if (!sid || !rid) return;
      const remaining = streamBufsRef.current[rid] || '';
      delete streamBufsRef.current[rid];
      delete sessionRunRef.current[sid];
      updateSS(sid, s => {
        const msgs = [...s.messages] as Array<{ role: string; parts?: Array<{ type: string; content?: string }> }>;
        if (remaining) {
          const last = msgs[msgs.length - 1];
          if (last?.role === 'turn') {
            msgs[msgs.length - 1] = { ...last, parts: [...(last.parts || []), { type: 'text', content: remaining }] };
          }
        }
        return { ...s, messages: msgs, streaming: '', running: false, liveRunId: null, liveStartedAt: null };
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

    ws.on('run.agent_start', () => {});

    ws.on('run.handoff', (p: { run_id: string; from: string; to: string }) => {
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      updateSS(sid, s => ({
        ...s, messages: [...s.messages, { role: 'system', content: 'Handoff: ' + p.from + ' → ' + p.to }],
      }));
    });

    ws.on('hook.event', (p: { run_id: string; hook: string; agent_name?: string; tool_name?: string; from?: string; to?: string; detail?: string }) => {
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      updateSS(sid, s => {
        const events = s.traceRuns[p.run_id] || [];
        const ev: TraceEvent = {
          kind: 'hook', name: p.hook,
          agent: p.agent_name || '', tool: p.tool_name || '',
          from: p.from || '', to: p.to || '', detail: p.detail || '',
        };
        return { ...s, traceRuns: { ...s.traceRuns, [p.run_id]: [...events, ev] } };
      });
    });

    ws.on('trace.span', (p: { run_id: string; name: string; type?: string; span_id?: string; started_at?: string; ended_at?: string; data?: unknown }) => {
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
          span_id: p.span_id, duration, data: p.data,
        };
        return { ...s, traceRuns: { ...s.traceRuns, [p.run_id]: [...events, ev] } };
      });
    });

    ws.connect();
    return () => ws.close();
  }, [updateSS, reloadMessages]);

  return { wsRef, sessionRunRef, loadSession, deleteSession };
}
