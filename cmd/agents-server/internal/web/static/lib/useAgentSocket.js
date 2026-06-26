import React from 'react';
import { WSClient } from '/lib/ws.js';
import { buildTimeline, formatHookDetail, patchToolCall } from '/lib/timeline.js';
import { api } from '/lib/api.js';

const { useCallback, useEffect, useRef } = React;

function defaultSS() {
  return { messages: [], streaming: '', running: false, traceRuns: {}, liveRunId: null, loaded: false };
}

export function useAgentSocket(updateSS) {
  const wsRef = useRef(null);
  const runMapRef = useRef({});
  const sessionRunRef = useRef({});
  const streamBufsRef = useRef({});
  const loadedRef = useRef(new Set());

  const reloadMessages = useCallback((sid) => {
    api.sessions.messages(sid).then(msgs => {
      const timeline = buildTimeline(msgs);
      updateSS(sid, s => ({ ...s, messages: timeline }));
    });
  }, [updateSS]);

  const loadSession = useCallback((sid) => {
    if (!sid || loadedRef.current.has(sid)) return;
    loadedRef.current.add(sid);
    api.sessions.messages(sid).then(msgs => {
      const timeline = buildTimeline(msgs);
      updateSS(sid, s => s.messages.length > 0 ? s : { ...s, messages: timeline, loaded: true });
    });
    api.sessions.traces(sid).then(events => {
      if (!events || events.length === 0) return;
      const runs = {};
      for (const ev of events) {
        const rid = ev.run_id || 'unknown';
        if (!runs[rid]) runs[rid] = [];
        runs[rid].push(ev);
      }
      updateSS(sid, s => Object.keys(s.traceRuns).length > 0 ? s : { ...s, traceRuns: runs });
    }).catch(() => {});
  }, [updateSS]);

  const deleteSession = useCallback((deletedId) => {
    loadedRef.current.delete(deletedId);
  }, []);

  useEffect(() => {
    const ws = new WSClient();
    wsRef.current = ws;

    ws.on('run.started', (p) => {
      const sid = p.session_id;
      if (!sid) return;
      runMapRef.current[p.run_id] = sid;
      sessionRunRef.current[sid] = p.run_id;
      streamBufsRef.current[p.run_id] = '';
      if (!loadedRef.current.has(sid)) loadedRef.current.add(sid);
      updateSS(sid, s => ({
        ...s, running: true, liveRunId: p.run_id, loaded: true,
        messages: [...s.messages, { role: 'turn', parts: [] }],
        traceRuns: { ...s.traceRuns, [p.run_id]: [] },
      }));
    });

    ws.on('run.step', (p) => {
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      streamBufsRef.current[p.run_id] = (streamBufsRef.current[p.run_id] || '') + p.delta;
      updateSS(sid, s => ({ ...s, streaming: streamBufsRef.current[p.run_id] }));
    });

    ws.on('run.output', (p) => {
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      const text = p.final_output || streamBufsRef.current[p.run_id] || '';
      delete streamBufsRef.current[p.run_id];
      delete runMapRef.current[p.run_id];
      delete sessionRunRef.current[sid];
      updateSS(sid, s => {
        const msgs = [...s.messages];
        if (text) {
          const last = msgs[msgs.length - 1];
          if (last?.role === 'turn') {
            msgs[msgs.length - 1] = { ...last, parts: [...last.parts, { type: 'text', content: text }] };
          }
        }
        return { ...s, messages: msgs, streaming: '', running: false, liveRunId: null };
      });
      reloadMessages(sid);
    });

    ws.on('run.error', (p) => {
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      const remaining = streamBufsRef.current[p.run_id] || '';
      delete streamBufsRef.current[p.run_id];
      delete runMapRef.current[p.run_id];
      delete sessionRunRef.current[sid];
      updateSS(sid, s => {
        const msgs = [...s.messages];
        const last = msgs[msgs.length - 1];
        if (last?.role === 'turn') {
          const parts = [...last.parts];
          if (remaining) parts.push({ type: 'text', content: remaining });
          msgs[msgs.length - 1] = { ...last, parts };
        } else {
          msgs.push({ role: 'system', content: 'Error: ' + p.message });
        }
        return { ...s, messages: msgs, streaming: '', running: false, liveRunId: null, lastError: p.message };
      });
      reloadMessages(sid);
    });

    ws.on('run.cancelled', (p) => {
      const rid = p?.run_id;
      const sid = rid ? runMapRef.current[rid] : null;
      if (!sid) return;
      const remaining = streamBufsRef.current[rid] || '';
      delete streamBufsRef.current[rid];
      delete runMapRef.current[rid];
      delete sessionRunRef.current[sid];
      updateSS(sid, s => {
        const msgs = [...s.messages];
        if (remaining) {
          const last = msgs[msgs.length - 1];
          if (last?.role === 'turn') {
            msgs[msgs.length - 1] = { ...last, parts: [...last.parts, { type: 'text', content: remaining }] };
          }
        }
        return { ...s, messages: msgs, streaming: '', running: false, liveRunId: null };
      });
      reloadMessages(sid);
    });

    ws.on('run.tool_call', (p) => {
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      const flushed = streamBufsRef.current[p.run_id] || '';
      streamBufsRef.current[p.run_id] = '';
      const tc = { tool_call_id: p.tool_call_id, tool_name: p.tool_name, arguments: p.arguments, needs_approval: p.needs_approval, status: null, output: null };
      updateSS(sid, s => {
        const msgs = [...s.messages];
        const last = msgs[msgs.length - 1];
        if (last?.role !== 'turn') return s;
        const parts = [...last.parts];
        if (flushed) parts.push({ type: 'text', content: flushed });
        const lastPart = parts[parts.length - 1];
        if (lastPart?.type === 'tools') {
          parts[parts.length - 1] = { ...lastPart, toolCalls: [...lastPart.toolCalls, tc] };
        } else {
          parts.push({ type: 'tools', toolCalls: [tc] });
        }
        msgs[msgs.length - 1] = { ...last, parts };
        return { ...s, messages: msgs, streaming: '' };
      });
    });

    ws.on('run.tool_result', (p) => {
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      updateSS(sid, s => {
        const patched = patchToolCall(s.messages, p.tool_call_id, { output: p.output, status: 'completed' });
        return patched ? { ...s, messages: patched } : s;
      });
    });

    ws.on('run.agent_start', () => {});

    ws.on('run.handoff', (p) => {
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      updateSS(sid, s => ({
        ...s, messages: [...s.messages, { role: 'system', content: 'Handoff: ' + p.from + ' → ' + p.to }],
      }));
    });

    ws.on('hook.event', (p) => {
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      updateSS(sid, s => {
        const events = s.traceRuns[p.run_id] || [];
        return { ...s, traceRuns: { ...s.traceRuns, [p.run_id]: [...events, { kind: 'hook', name: p.hook, detail: formatHookDetail(p) }] } };
      });
    });

    ws.on('trace.span', (p) => {
      const sid = runMapRef.current[p.run_id];
      if (!sid) return;
      updateSS(sid, s => {
        const events = s.traceRuns[p.run_id] || [];
        return { ...s, traceRuns: { ...s.traceRuns, [p.run_id]: [...events, { kind: 'span', name: p.name, detail: p.type || '', span_id: p.span_id }] } };
      });
    });

    ws.connect();
    return () => ws.close();
  }, [updateSS, reloadMessages]);

  return { wsRef, sessionRunRef, loadSession, deleteSession };
}
