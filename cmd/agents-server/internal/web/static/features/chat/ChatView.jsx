import React from 'react';
import { api } from '/lib/api.js';
import { renderMarkdown, splitMermaidBlocks, sanitizeSVG } from '/lib/markdown.js';
import { useScrollToBottom, useApi } from '/lib/hooks.js';
import { MessageBubble } from '/features/chat/MessageBubble.jsx';
import { MessageInput } from '/features/chat/MessageInput.jsx';
import { ToolCallCard } from '/features/chat/ToolCallCard.jsx';
import { TracePanel } from '/features/chat/TracePanel.jsx';
import { iconChevronRight, iconFork, iconCopy, iconCheck, iconSync, iconEdit, iconChatBadge } from '/lib/icons.js';

const { useState, useEffect, useCallback, useMemo, useRef } = React;
const h = React.createElement;

let mermaidMod = null;
let mermaidTheme = null;
let mermaidIdSeq = 0;

function primerThemeVars() {
  const s = getComputedStyle(document.documentElement);
  const v = (n) => s.getPropertyValue(n).trim();
  return {
    fontFamily: v('--font-sans'),
    fontSize: '14px',

    primaryColor: v('--color-canvas-subtle'),
    primaryBorderColor: v('--color-border-default'),
    primaryTextColor: v('--color-fg-default'),
    secondaryColor: v('--color-neutral-muted'),
    secondaryBorderColor: v('--color-border-default'),
    secondaryTextColor: v('--color-fg-default'),
    tertiaryColor: v('--color-success-subtle'),
    tertiaryBorderColor: v('--color-border-default'),
    tertiaryTextColor: v('--color-fg-default'),

    lineColor: v('--color-fg-muted'),
    textColor: v('--color-fg-default'),
    mainBkg: v('--color-canvas-subtle'),
    nodeBorder: v('--color-border-default'),
    nodeTextColor: v('--color-fg-default'),
    clusterBkg: v('--color-canvas-default'),
    clusterBorder: v('--color-border-muted'),
    titleColor: v('--color-fg-default'),
    edgeLabelBackground: v('--color-canvas-default'),

    actorBkg: v('--color-canvas-subtle'),
    actorBorder: v('--color-border-default'),
    actorTextColor: v('--color-fg-default'),
    signalColor: v('--color-fg-default'),
    signalTextColor: v('--color-fg-default'),
    labelBoxBkgColor: v('--color-canvas-subtle'),
    labelBoxBorderColor: v('--color-border-default'),
    labelTextColor: v('--color-fg-default'),
    noteBkgColor: v('--color-attention-subtle'),
    noteBorderColor: v('--color-border-default'),
    noteTextColor: v('--color-fg-default'),
    activationBorderColor: v('--color-border-default'),
    activationBkgColor: v('--color-canvas-subtle'),
  };
}

async function ensureMermaid() {
  if (!mermaidMod) {
    mermaidMod = (await import('mermaid')).default;
  }
  const cur = document.documentElement.getAttribute('data-color-mode') || 'light';
  if (mermaidTheme !== cur) {
    mermaidTheme = cur;
    mermaidMod.initialize({
      startOnLoad: false,
      theme: 'base',
      themeVariables: primerThemeVars(),
    });
  }
  return mermaidMod;
}

const MERMAID_CACHE_MAX = 100;
const mermaidCache = new Map();
function mermaidCacheSet(key, value) {
  if (mermaidCache.size >= MERMAID_CACHE_MAX) {
    const first = mermaidCache.keys().next().value;
    mermaidCache.delete(first);
  }
  mermaidCache.set(key, value);
}

function MermaidBlock({ source }) {
  const [colorMode, setColorMode] = useState(() =>
    document.documentElement.getAttribute('data-color-mode') || 'light'
  );
  const cacheKey = source + '\0' + colorMode;
  const [svg, setSvg] = useState(() => mermaidCache.get(cacheKey) || null);

  useEffect(() => {
    const obs = new MutationObserver(() => {
      setColorMode(document.documentElement.getAttribute('data-color-mode') || 'light');
    });
    obs.observe(document.documentElement, { attributes: true, attributeFilter: ['data-color-mode'] });
    return () => obs.disconnect();
  }, []);

  useEffect(() => {
    const cached = mermaidCache.get(cacheKey);
    if (cached) { setSvg(cached); return; }
    let cancelled = false;
    (async () => {
      try {
        const mermaid = await ensureMermaid();
        const { svg: rendered } = await mermaid.render(`m${++mermaidIdSeq}`, source);
        if (!cancelled) {
          const safe = sanitizeSVG(rendered);
          mermaidCacheSet(cacheKey, safe);
          setSvg(safe);
        }
      } catch {
        // invalid syntax
      }
    })();
    return () => { cancelled = true; };
  }, [source, cacheKey]);

  if (svg) return h('div', { className: 'mermaid-diagram', dangerouslySetInnerHTML: { __html: svg } });
  return h('pre', { className: 'mermaid-pending' }, source);
}

function TextContent({ content }) {
  const segments = useMemo(() => splitMermaidBlocks(content), [content]);
  if (segments.length === 1 && segments[0].type === 'md') {
    return h('div', { className: 'turn-text markdown-body', dangerouslySetInnerHTML: { __html: renderMarkdown(content) } });
  }
  return h('div', { className: 'turn-text markdown-body' },
    segments.map((seg, i) =>
      seg.type === 'mermaid'
        ? h(MermaidBlock, { key: `m${i}`, source: seg.text })
        : h('div', { key: `t${i}`, dangerouslySetInnerHTML: { __html: renderMarkdown(seg.text) } })
    )
  );
}

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
      iconChevronRight({ className: 'process-icon' }),
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

function TurnBlock({ parts, streaming, isLive, onApprove, onReject, turnText, onRegenerate, running }) {
  const isEmpty = parts.length === 0 && !streaming;
  const [copied, setCopied] = useState(false);

  const handleCopy = useCallback(() => {
    if (!turnText) return;
    navigator.clipboard.writeText(turnText).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    });
  }, [turnText]);

  return h('div', { className: 'message message-turn' },
    parts.map((part, i) => {
      if (part.type === 'text') {
        return h(TextContent, { key: 'p-' + i, content: part.content });
      }
      if (part.type === 'tools') {
        return h(ProcessGroup, { key: 'p-' + i, toolCalls: part.toolCalls, onApprove, onReject });
      }
      return null;
    }),
    streaming && h('div', { className: 'turn-text markdown-body streaming', dangerouslySetInnerHTML: { __html: renderMarkdown(streaming + '▋') } }),
    isLive && isEmpty && h('div', { className: 'thinking-indicator' },
      h('div', { className: 'thinking-dots' },
        h('span', null), h('span', null), h('span', null),
      ),
    ),
    !isLive && turnText && h('div', { className: 'turn-actions' },
      h('button', {
        className: 'turn-action-btn' + (copied ? ' copied' : ''),
        title: copied ? 'Copied!' : 'Copy response',
        onClick: handleCopy,
      }, copied ? iconCheck() : iconCopy()),
      !running && onRegenerate && h('button', {
        className: 'turn-action-btn',
        title: 'Regenerate',
        onClick: onRegenerate,
      }, iconSync()),
    ),
  );
}


function UserMessage({ content, messageId, onFork, onSend, running }) {
  const [editing, setEditing] = useState(false);
  const [editText, setEditText] = useState(content);

  const handleSubmit = useCallback(() => {
    const trimmed = editText.trim();
    if (!trimmed) return;
    setEditing(false);
    onSend(trimmed);
  }, [editText, onSend]);

  if (editing) {
    return h('div', { className: 'message message-user message-editing' },
      h('textarea', {
        className: 'message-edit-textarea',
        value: editText,
        onChange: e => setEditText(e.target.value),
        onKeyDown: e => {
          if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); handleSubmit(); }
          if (e.key === 'Escape') { setEditing(false); setEditText(content); }
        },
        autoFocus: true,
        rows: 3,
      }),
      h('div', { className: 'message-edit-actions' },
        h('button', { className: 'btn btn-sm', onClick: () => { setEditing(false); setEditText(content); } }, 'Cancel'),
        h('button', { className: 'btn btn-sm btn-primary', onClick: handleSubmit, disabled: !editText.trim() }, 'Send'),
      ),
    );
  }

  return h('div', { className: 'message message-user message-forkable' },
    h('div', { className: 'message-body' }, content),
    h('div', { className: 'message-user-actions' },
      !running && h('button', {
        className: 'message-action-btn',
        title: 'Edit & resend',
        onClick: () => { setEditText(content); setEditing(true); },
      }, iconEdit()),
      messageId && onFork && h('button', {
        className: 'message-action-btn',
        title: 'Fork before this message',
        onClick: () => onFork(messageId),
      }, iconFork()),
    ),
  );
}

export function ChatView({ sessionId, messages, loaded, streaming, running, traceRuns, liveRunId, lastError, onSend, onCancel, onApprove, onReject, onFork, settingsReloadKey }) {
  const [toast, setToast] = useState(null);
  const [agentConfigId, setAgentConfigId] = useState('');
  const [sandboxId, setSandboxId] = useState('');
  const { data: agentConfigs, reload: reloadAgents } = useApi(() => api.agents.list());
  const { data: sandboxConfigs, reload: reloadSandboxes } = useApi(() => api.sandboxes.list());

  useEffect(() => {
    if (!agentConfigs || agentConfigs.length === 0) return;
    const valid = agentConfigs.some(a => a.id === agentConfigId);
    if (!valid) {
      setAgentConfigId(agentConfigs[0].id);
    }
  }, [agentConfigs, agentConfigId]);

  useEffect(() => {
    if (settingsReloadKey) { reloadAgents(); reloadSandboxes(); }
  }, [settingsReloadKey, reloadAgents, reloadSandboxes]);

  const scrollRef = useScrollToBottom(messages.length + streaming, sessionId);

  const toastTimerRef = useRef(null);
  const showToast = useCallback((msg, type) => {
    if (toastTimerRef.current) clearTimeout(toastTimerRef.current);
    setToast({ msg, type });
    toastTimerRef.current = setTimeout(() => { setToast(null); toastTimerRef.current = null; }, 4000);
  }, []);

  useEffect(() => {
    if (lastError) showToast(lastError, 'error');
  }, [lastError, showToast]);

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
      h('div', { className: 'chat-empty-badge' }, iconChatBadge()),
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

  const loading = sessionId && !loaded && messages.length === 0;

  return h('div', { className: 'chat-main' },
    h('div', { ref: scrollRef, className: 'chat-messages', onClick: handleCopyClick },
      loading ? null : messages.map((m, i) => {
        if (m.role === 'turn') {
          const isLive = running && i === messages.length - 1;
          const turnText = m.parts.filter(p => p.type === 'text').map(p => p.content).join('\n\n');
          let prevUserContent = null;
          for (let j = i - 1; j >= 0; j--) {
            if (messages[j].role === 'user') { prevUserContent = messages[j].content; break; }
          }
          return h(TurnBlock, {
            key: 'turn-' + i,
            parts: m.parts,
            streaming: isLive ? streaming : null,
            isLive,
            onApprove: onApprove,
            onReject: onReject,
            turnText,
            onRegenerate: prevUserContent ? () => handleSend(prevUserContent) : null,
            running,
          });
        }
        if (m.role === 'user') {
          return h(UserMessage, {
            key: i,
            content: m.content,
            messageId: m.messageId,
            onFork: onFork,
            onSend: handleSend,
            running,
          });
        }
        return h(MessageBubble, { key: i, role: m.role, content: m.content });
      }),
    ),

    h(TracePanel, { traceRuns, liveRunId }),

    h(MessageInput, { onSend: handleSend, onCancel: handleCancel, disabled: running || !agentConfigId, running, footer: inputFooter }),

    toast && h('div', {
      className: 'Toast ' + (toast.type === 'error' ? 'Toast-error' : 'Toast-info'),
    }, toast.msg),
  );
}
