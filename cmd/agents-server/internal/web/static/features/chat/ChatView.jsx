import React from 'react';
import { api } from '/lib/api.js';
import { renderMarkdown } from '/lib/markdown.js';
import { useScrollToBottom, useApi } from '/lib/hooks.js';
import { MessageBubble } from '/features/chat/MessageBubble.jsx';
import { MessageInput } from '/features/chat/MessageInput.jsx';
import { ToolCallCard } from '/features/chat/ToolCallCard.jsx';

const { useState, useEffect, useCallback, useMemo } = React;
const h = React.createElement;

function ProcessGroup({ toolCalls, onApprove, onReject }) {
  const [expanded, setExpanded] = useState(false);
  const count = toolCalls.length;
  if (count === 0) return null;

  const pendingCount = toolCalls.filter(tc => tc.needs_approval && !tc.status).length;
  const completedCount = toolCalls.filter(tc => tc.status === 'completed' || tc.output).length;
  const isRunning = completedCount < count && pendingCount === 0;

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
      pendingCount > 0 && h('span', { className: 'process-status Label Label--accent' }, pendingCount + ' pending'),
      isRunning && h('span', { className: 'process-status Label Label--secondary' }, 'running...'),
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

function TurnBlock({ parts, streaming, isLive, onApprove, onReject }) {
  const isEmpty = parts.length === 0 && !streaming;
  return h('div', { className: 'message message-turn' },
    parts.map((part, i) => {
      if (part.type === 'text') {
        return h('div', { key: 'p-' + i, className: 'turn-text markdown-body', dangerouslySetInnerHTML: { __html: renderMarkdown(part.content) } });
      }
      if (part.type === 'tools') {
        return h(ProcessGroup, { key: 'p-' + i, toolCalls: part.toolCalls, onApprove, onReject });
      }
      return null;
    }),
    streaming && h('div', { className: 'turn-text markdown-body', dangerouslySetInnerHTML: { __html: renderMarkdown(streaming + '▋') } }),
    isLive && isEmpty && h('div', { className: 'thinking-indicator' },
      h('div', { className: 'thinking-dots' },
        h('span', null), h('span', null), h('span', null),
      ),
    ),
  );
}

function iconFork() {
  return h('svg', { viewBox: '0 0 16 16', fill: 'currentColor', width: 14, height: 14 },
    h('path', { d: 'M5 3.254V3.25v.005a.75.75 0 1 1 0-.005Zm.45 1.9a2.25 2.25 0 1 0-1.95.218v5.256a2.25 2.25 0 1 0 1.5 0V7.123A5.735 5.735 0 0 0 9.25 9h1.378a2.251 2.251 0 1 0 0-1.5H9.25a4.25 4.25 0 0 1-3.8-2.346ZM12.75 9a.75.75 0 1 1 0-1.5.75.75 0 0 1 0 1.5Zm-8.5 3.5a.75.75 0 1 1 0-1.5.75.75 0 0 1 0 1.5Z' }),
  );
}

export function ChatView({ sessionId, messages, streaming, running, traceRuns, liveRunId, lastError, onSend, onCancel, onApprove, onReject, onFork, settingsReloadKey }) {
  const [toast, setToast] = useState(null);
  const [agentConfigId, setAgentConfigId] = useState('');
  const [sandboxId, setSandboxId] = useState('');
  const [showTrace, setShowTrace] = useState(false);
  const [expandedRuns, setExpandedRuns] = useState({});
  const { data: agentConfigs, reload: reloadAgents } = useApi(() => api.agents.list());
  const { data: sandboxConfigs, reload: reloadSandboxes } = useApi(() => api.sandboxes.list());

  useEffect(() => {
    if (!agentConfigs || agentConfigs.length === 0) return;
    const valid = agentConfigs.some(a => a.id === agentConfigId);
    if (!valid) {
      setAgentConfigId(agentConfigs[0].id);
    }
  }, [agentConfigs]);

  useEffect(() => {
    if (settingsReloadKey) { reloadAgents(); reloadSandboxes(); }
  }, [settingsReloadKey]);

  const scrollRef = useScrollToBottom(messages.length + streaming, sessionId);

  const showToast = useCallback((msg, type) => {
    setToast({ msg, type });
    setTimeout(() => setToast(null), 4000);
  }, []);

  useEffect(() => {
    if (lastError) showToast(lastError, 'error');
  }, [lastError, showToast]);

  useEffect(() => {
    const newRuns = Object.keys(traceRuns).filter(rid => !(rid in expandedRuns));
    if (newRuns.length) {
      setExpandedRuns(prev => {
        const next = { ...prev };
        newRuns.forEach(rid => { next[rid] = true; });
        return next;
      });
    }
  }, [traceRuns]);

  const handleCopyClick = useCallback((e) => {
    const btn = e.target.closest('.btn-copy');
    if (!btn) return;
    const code = btn.getAttribute('data-code')
      ?.replace(/&amp;/g, '&').replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&quot;/g, '"');
    if (!code) return;
    navigator.clipboard.writeText(code).then(() => {
      btn.classList.add('copied');
      const svg = btn.innerHTML;
      btn.innerHTML = '<svg viewBox="0 0 16 16" fill="currentColor" width="16" height="16"><path d="M13.78 4.22a.75.75 0 0 1 0 1.06l-7.25 7.25a.75.75 0 0 1-1.06 0L2.22 9.28a.751.751 0 0 1 .018-1.042.751.751 0 0 1 1.042-.018L6 10.94l6.72-6.72a.75.75 0 0 1 1.06 0Z"></path></svg>';
      setTimeout(() => { btn.classList.remove('copied'); btn.innerHTML = svg; }, 1500);
    });
  }, []);

  const handleSend = useCallback((text) => {
    if (!sessionId || !agentConfigId) return;
    onSend(text, agentConfigId, sandboxId);
  }, [sessionId, agentConfigId, sandboxId, onSend]);

  const handleCancel = useCallback(() => {
    onCancel();
    showToast('Run cancelled', 'info');
  }, [onCancel, showToast]);

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
    h('div', { ref: scrollRef, className: 'chat-messages', onClick: handleCopyClick },
      messages.map((m, i) => {
        if (m.role === 'turn') {
          const isLive = running && i === messages.length - 1;
          return h(TurnBlock, {
            key: 'turn-' + i,
            parts: m.parts,
            streaming: isLive ? streaming : null,
            isLive,
            onApprove: onApprove,
            onReject: onReject,
          });
        }
        if (m.role === 'user' && m.messageId && onFork) {
          return h('div', { key: i, className: 'message message-user message-forkable' },
            h('div', { className: 'message-body' }, m.content),
            h('button', {
              className: 'message-fork-btn',
              title: 'Fork before this message',
              onClick: () => onFork(m.messageId),
            }, iconFork()),
          );
        }
        return h(MessageBubble, { key: i, role: m.role, content: m.content });
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
              isLive && h('span', { className: 'Label Label--accent', style: { fontSize: '10px' } }, 'live'),
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

    h(MessageInput, { onSend: handleSend, onCancel: handleCancel, disabled: running || !agentConfigId, running, footer: inputFooter }),

    toast && h('div', {
      className: 'Toast ' + (toast.type === 'error' ? 'Toast-error' : 'Toast-info'),
    }, toast.msg),
  );
}
