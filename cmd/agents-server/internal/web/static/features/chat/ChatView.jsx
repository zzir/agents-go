import React from 'react';
import { WSClient } from '/lib/ws.js';
import { api } from '/lib/api.js';
import { useScrollToBottom, useApi } from '/lib/hooks.js';
import { MessageBubble } from '/features/chat/MessageBubble.jsx';
import { MessageInput } from '/features/chat/MessageInput.jsx';
import { ToolCallCard } from '/features/chat/ToolCallCard.jsx';

const { useState, useEffect, useRef, useCallback } = React;
const h = React.createElement;

function ProcessGroup({ toolCalls, onApprove, onReject }) {
  const [expanded, setExpanded] = useState(false);
  const count = toolCalls.length;
  if (count === 0) return null;

  const pendingCount = toolCalls.filter(tc => tc.needs_approval && !tc.status).length;
  const completedCount = toolCalls.filter(tc => tc.status === 'completed' || tc.output).length;
  const isRunning = completedCount < count && pendingCount === 0;

  // Auto-expand if there are pending approvals
  const shouldShow = expanded || pendingCount > 0;

  return h('div', { className: 'process-group' },
    h('div', {
      className: 'process-group-toggle' + (shouldShow ? ' expanded' : ''),
      onClick: () => setExpanded(!expanded),
    },
      h('svg', { className: 'process-icon', viewBox: '0 0 16 16', fill: 'currentColor' },
        h('path', { d: 'M6.22 3.22a.75.75 0 0 1 1.06 0l4.25 4.25a.75.75 0 0 1 0 1.06l-4.25 4.25a.751.751 0 0 1-1.042-.018.751.751 0 0 1-.018-1.042L9.94 8 6.22 4.28a.75.75 0 0 1 0-1.06Z' }),
      ),
      h('span', null, count + ' tool call' + (count > 1 ? 's' : '')),
      pendingCount > 0 && h('span', { className: 'process-status Label Label-accent' }, pendingCount + ' pending'),
      isRunning && h('span', { className: 'process-status Label Label-default' }, 'running...'),
    ),
    shouldShow && h('div', { className: 'process-group-body' },
      toolCalls.map(tc =>
        h(ToolCallCard, {
          key: tc.tool_call_id,
          toolCall: tc,
          onApprove: onApprove,
          onReject: onReject,
        }),
      ),
    ),
  );
}

export function ChatView({ sessionId, onSessionUpdated, settingsReloadKey }) {
  const [messages, setMessages] = useState([]);
  const [toolCalls, setToolCalls] = useState({});
  const [streaming, setStreaming] = useState('');
  const [running, setRunning] = useState(false);
  const [toast, setToast] = useState(null);
  const [agentConfigId, setAgentConfigId] = useState('');
  const [sandboxId, setSandboxId] = useState('');
  const [traceRuns, setTraceRuns] = useState({});
  const [liveRunId, setLiveRunId] = useState(null);
  const [showTrace, setShowTrace] = useState(false);
  const [expandedRuns, setExpandedRuns] = useState({});
  const { data: agentConfigs, reload: reloadAgents } = useApi(() => api.agents.list());
  const { data: sandboxConfigs, reload: reloadSandboxes } = useApi(() => api.sandboxes.list());

  useEffect(() => {
    if (agentConfigs && agentConfigs.length > 0 && !agentConfigId) {
      setAgentConfigId(agentConfigs[0].id);
    }
  }, [agentConfigs]);

  // Settings closed — agents/sandboxes may have changed; re-fetch the lists
  // (they were otherwise only loaded once on mount).
  useEffect(() => {
    if (settingsReloadKey) { reloadAgents(); reloadSandboxes(); }
  }, [settingsReloadKey]);

  const wsRef = useRef(null);
  const runIdRef = useRef(null);
  const scrollRef = useScrollToBottom(messages.length + streaming, sessionId);

  const showToast = useCallback((msg, type) => {
    setToast({ msg, type });
    setTimeout(() => setToast(null), 4000);
  }, []);

  useEffect(() => {
    if (!sessionId) return;
    setMessages([]);
    setStreaming('');
    setToolCalls({});
    setTraceRuns({});
    setLiveRunId(null);

    api.sessions.messages(sessionId).then(msgs => {
      if (!msgs) return;
      const timeline = [];
      const pendingTC = {};
      let tcBatch = [];
      const flushBatch = () => {
        if (tcBatch.length > 0) {
          timeline.push({ role: 'tools', toolCalls: tcBatch });
          tcBatch = [];
        }
      };
      for (const m of msgs) {
        if (m.role === 'tool_call') {
          try {
            const item = JSON.parse(m.item);
            const tc = {
              tool_call_id: item.call_id,
              tool_name: item.name,
              arguments: item.arguments || '',
              output: null,
              status: null,
            };
            pendingTC[item.call_id] = tc;
            tcBatch.push(tc);
          } catch (_) {}
        } else if (m.role === 'tool_output') {
          try {
            const item = JSON.parse(m.item);
            if (pendingTC[item.call_id]) {
              pendingTC[item.call_id].output = item.output || m.content;
              pendingTC[item.call_id].status = 'completed';
            }
          } catch (_) {}
        } else if (m.content) {
          flushBatch();
          timeline.push({ role: m.role, content: m.content });
        }
      }
      flushBatch();
      setMessages(timeline);
    });

    api.sessions.traces(sessionId).then(events => {
      if (!events || events.length === 0) return;
      const runs = {};
      for (const ev of events) {
        const rid = ev.run_id || 'unknown';
        if (!runs[rid]) runs[rid] = [];
        runs[rid].push(ev);
      }
      setTraceRuns(runs);
    }).catch(() => {});
  }, [sessionId]);

  useEffect(() => {
    const ws = new WSClient();
    wsRef.current = ws;

    ws.on('run.started', (payload) => {
      setRunning(true);
      setStreaming('');
      runIdRef.current = payload.run_id;
      setLiveRunId(payload.run_id);
      setTraceRuns(prev => ({ ...prev, [payload.run_id]: [] }));
      setExpandedRuns(prev => ({ ...prev, [payload.run_id]: true }));
    });
    ws.on('run.step', (payload) => {
      setStreaming(prev => prev + payload.delta);
    });
    ws.on('run.output', (payload) => {
      setStreaming('');
      setRunning(false);
      setLiveRunId(null);
      runIdRef.current = null;
      setToolCalls(prevTC => {
        const tcList = Object.values(prevTC);
        if (tcList.length > 0) {
          setMessages(prev => [
            ...prev,
            { role: 'tools', toolCalls: tcList },
            { role: 'assistant', content: payload.final_output },
          ]);
        } else {
          setMessages(prev => [...prev, { role: 'assistant', content: payload.final_output }]);
        }
        return {};
      });
    });
    ws.on('run.error', (payload) => {
      setStreaming('');
      setRunning(false);
      setLiveRunId(null);
      runIdRef.current = null;
      showToast(payload.message, 'error');
      setToolCalls(prevTC => {
        const tcList = Object.values(prevTC);
        if (tcList.length > 0) {
          setMessages(prev => [
            ...prev,
            { role: 'tools', toolCalls: tcList },
            { role: 'system', content: 'Error: ' + payload.message },
          ]);
        } else {
          setMessages(prev => [...prev, { role: 'system', content: 'Error: ' + payload.message }]);
        }
        return {};
      });
    });
    ws.on('run.tool_call', (payload) => {
      setToolCalls(prev => ({
        ...prev,
        [payload.tool_call_id]: {
          tool_call_id: payload.tool_call_id,
          tool_name: payload.tool_name,
          arguments: payload.arguments,
          needs_approval: payload.needs_approval,
          status: null,
          output: null,
        }
      }));
    });
    ws.on('run.tool_result', (payload) => {
      setToolCalls(prev => {
        const updated = { ...prev };
        const key = payload.tool_call_id || Object.keys(updated).find(k => !updated[k].output);
        if (key && updated[key]) {
          updated[key] = { ...updated[key], output: payload.output, status: 'completed' };
        }
        return updated;
      });
    });
    ws.on('run.agent_start', () => {});
    ws.on('run.handoff', (payload) => {
      setMessages(prev => [...prev, {
        role: 'system',
        content: 'Handoff: ' + payload.from + ' → ' + payload.to,
      }]);
    });
    ws.on('hook.event', (payload) => {
      const rid = runIdRef.current;
      if (!rid) return;
      setTraceRuns(prev => {
        const events = prev[rid] || [];
        return { ...prev, [rid]: [...events, { kind: 'hook', name: payload.hook, detail: formatHookDetail(payload) }] };
      });
    });
    ws.on('trace.span', (payload) => {
      const rid = runIdRef.current;
      if (!rid) return;
      setTraceRuns(prev => {
        const events = prev[rid] || [];
        return { ...prev, [rid]: [...events, { kind: 'span', name: payload.name, detail: payload.type || '', span_id: payload.span_id }] };
      });
    });
    ws.on('session.title_updated', () => {
      if (onSessionUpdated) onSessionUpdated();
    });
    ws.connect();
    return () => ws.close();
  }, [showToast]);

  const handleSend = useCallback((text) => {
    if (!sessionId || !wsRef.current || !agentConfigId) return;
    setMessages(prev => [...prev, { role: 'user', content: text }]);
    setToolCalls({});
    const payload = {
      session_id: sessionId,
      input: text,
      agent_config_id: agentConfigId,
    };
    if (sandboxId) payload.sandbox_id = sandboxId;
    wsRef.current.send('run.create', payload);
  }, [sessionId, agentConfigId, sandboxId]);

  const handleCancel = useCallback(() => {
    if (!wsRef.current || !runIdRef.current) return;
    wsRef.current.send('run.cancel', { run_id: runIdRef.current });
    if (streaming) {
      setMessages(prev => [...prev, { role: 'assistant', content: streaming }]);
    }
    setStreaming('');
    setRunning(false);
    runIdRef.current = null;
    showToast('Run cancelled', 'info');
  }, [streaming, showToast]);

  const handleApprove = useCallback((toolCallId) => {
    if (!wsRef.current) return;
    setToolCalls(prev => ({
      ...prev,
      [toolCallId]: { ...prev[toolCallId], status: 'approved' }
    }));
    wsRef.current.send('tool.approve', { tool_call_id: toolCallId });
  }, []);

  const handleReject = useCallback((toolCallId) => {
    if (!wsRef.current) return;
    setToolCalls(prev => ({
      ...prev,
      [toolCallId]: { ...prev[toolCallId], status: 'rejected' }
    }));
    wsRef.current.send('tool.reject', { tool_call_id: toolCallId });
  }, []);

  if (!sessionId) {
    return h('div', { className: 'chat-empty' },
      h('div', { className: 'chat-empty-badge' },
        h('svg', { viewBox: '0 0 16 16', fill: 'currentColor', 'aria-hidden': 'true' },
          h('path', { d: 'M1.75 1h12.5c.966 0 1.75.784 1.75 1.75v9.5A1.75 1.75 0 0 1 14.25 14H8.061l-2.574 2.573A1.458 1.458 0 0 1 3 15.543V14H1.75A1.75 1.75 0 0 1 0 12.25v-9.5C0 1.784.784 1 1.75 1ZM1.5 2.75v9.5c0 .138.112.25.25.25h2a.75.75 0 0 1 .75.75v2.19l2.72-2.72a.749.749 0 0 1 .53-.22h6.5a.25.25 0 0 0 .25-.25v-9.5a.25.25 0 0 0-.25-.25H1.75a.25.25 0 0 0-.25.25Z' }),
        ),
      ),
      h('div', { className: 'chat-empty-title' }, 'Start a conversation'),
      h('div', { className: 'chat-empty-sub' }, 'Pick a chat from the sidebar, or create a new one to begin.'),
    );
  }

  const toolCallList = Object.values(toolCalls);

  const inputFooter = h('div', { className: 'chat-input-footer' },
    agentConfigs && agentConfigs.length > 0
      ? h('label', { className: 'chat-input-footer-item' },
          h('span', null, 'Agent'),
          h('select', {
            value: agentConfigId,
            onChange: e => setAgentConfigId(e.target.value),
          },
            agentConfigs.map(a => h('option', { key: a.id, value: a.id }, a.name)),
          ),
        )
      : h('span', { className: 'chat-input-footer-warn' }, 'No agents — go to Settings'),
    sandboxConfigs && sandboxConfigs.length > 0 && h('label', { className: 'chat-input-footer-item' },
      h('span', null, 'Sandbox'),
      h('select', {
        value: sandboxId,
        onChange: e => setSandboxId(e.target.value),
      },
        h('option', { value: '' }, 'None'),
        h('option', { value: '__all__' }, 'All'),
        sandboxConfigs.map(s => h('option', { key: s.id, value: s.id }, s.name)),
      ),
    ),
  );

  return h('div', { className: 'chat-main' },
    h('div', { ref: scrollRef, className: 'chat-messages' },
      messages.map((m, i) =>
        m.role === 'tools'
          ? h(ProcessGroup, { key: 'tc-' + i, toolCalls: m.toolCalls, onApprove: handleApprove, onReject: handleReject })
          : h(MessageBubble, { key: i, role: m.role, content: m.content }),
      ),
      toolCallList.length > 0 && !messages.some(m => m.role === 'tools' && m.toolCalls === toolCallList) && h(ProcessGroup, {
        toolCalls: toolCallList,
        onApprove: handleApprove,
        onReject: handleReject,
      }),
      running && !streaming && toolCallList.length === 0 && h('div', { className: 'thinking-indicator' },
        h('div', { className: 'thinking-dots' },
          h('span', null), h('span', null), h('span', null),
        ),
      ),
      streaming && h(MessageBubble, {
        role: 'assistant',
        content: streaming + '▋',
      }),
    ),

    Object.keys(traceRuns).length > 0 && h('div', { className: 'trace-panel' },
      h('div', {
        className: 'trace-toggle',
        onClick: () => setShowTrace(!showTrace),
      }, (showTrace ? '▾' : '▸') + ' Traces (' + Object.keys(traceRuns).length + ' run' + (Object.keys(traceRuns).length > 1 ? 's' : '') + ')'),
      showTrace && h('div', { style: { maxHeight: '200px', overflowY: 'auto', marginTop: '4px' } },
        Object.entries(traceRuns).map(([rid, events]) => {
          const isLive = rid === liveRunId;
          const isExpanded = expandedRuns[rid];
          const hookCount = events.filter(e => e.kind === 'hook').length;
          const spanCount = events.filter(e => e.kind === 'span').length;
          return h('div', { key: rid, style: { marginBottom: '4px' } },
            h('div', {
              className: 'trace-run-header',
              onClick: () => setExpandedRuns(prev => ({ ...prev, [rid]: !prev[rid] })),
              style: { cursor: 'pointer', display: 'flex', alignItems: 'center', gap: '6px', fontSize: '11px', padding: '2px 0' },
            },
              h('span', null, isExpanded ? '▾' : '▸'),
              h('span', { style: { fontFamily: 'monospace', color: 'var(--color-fg-muted)' } }, rid.slice(0, 8)),
              isLive && h('span', { className: 'Label Label-accent', style: { fontSize: '10px' } }, 'live'),
              h('span', { style: { color: 'var(--color-fg-subtle)' } },
                hookCount + ' hook' + (hookCount !== 1 ? 's' : '') + ', ' + spanCount + ' span' + (spanCount !== 1 ? 's' : '')),
            ),
            isExpanded && h('div', { style: { paddingLeft: '12px' } },
              events.map((e, i) =>
                h('div', { key: i, className: 'trace-event' },
                  h('span', { style: { color: e.kind === 'hook' ? 'var(--color-accent-fg)' : 'var(--color-success-fg)' } },
                    e.kind === 'hook' ? e.name : '◆ ' + e.name),
                  e.detail && h('span', { style: { color: 'var(--color-fg-subtle)', marginLeft: '6px' } }, e.detail),
                ),
              ),
            ),
          );
        }),
      ),
    ),

    h(MessageInput, { onSend: handleSend, onCancel: handleCancel, disabled: running, running, footer: inputFooter }),

    toast && h('div', {
      className: 'Toast ' + (toast.type === 'error' ? 'Toast-error' : 'Toast-info'),
    }, toast.msg),
  );
}

function formatHookDetail(ev) {
  const parts = [];
  if (ev.agent_name) parts.push(ev.agent_name);
  if (ev.tool_name) parts.push('→ ' + ev.tool_name);
  if (ev.from && ev.to) parts.push(ev.from + ' → ' + ev.to);
  if (ev.detail) parts.push(ev.detail);
  return parts.join(' ');
}
