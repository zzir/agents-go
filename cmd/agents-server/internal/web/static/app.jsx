import React from 'react';
import ReactDOM from 'react-dom/client';
import { ThemeProvider } from '/theme/ThemeProvider.jsx';
import { AppShell } from '/layout/AppShell.jsx';
import { SessionList } from '/features/sessions/SessionList.jsx';
import { ChatView } from '/features/chat/ChatView.jsx';
import { AgentConfigPanel } from '/features/agents/AgentConfigPanel.jsx';
import { McpServerPanel } from '/features/mcp/McpServerPanel.jsx';
import { SkillsPanel } from '/features/skills/SkillsPanel.jsx';
import { MemoryPanel } from '/features/memory/MemoryPanel.jsx';
import { SettingsPanel } from '/features/settings/SettingsPanel.jsx';
import { FileBrowser } from '/features/files/FileBrowser.jsx';
import { FileTree } from '/features/files/FileTree.jsx';
import { FileViewer } from '/features/files/FileViewer.jsx';
import { SandboxPanel } from '/features/sandbox/SandboxPanel.jsx';
import { WSClient } from '/lib/ws.js';
import { login, checkAuth, getToken, api } from '/lib/api.js';

const { useState, useCallback, useEffect, useRef, useMemo } = React;
const h = React.createElement;

const DIALOG_TABS = [
  { key: 'agents',   label: 'Agents',   comp: AgentConfigPanel },
  { key: 'sandbox',  label: 'Sandbox',  comp: SandboxPanel },
  { key: 'memory',   label: 'Memory',   comp: MemoryPanel },
  { key: 'mcp',      label: 'MCP',      comp: McpServerPanel },
  { key: 'skills',   label: 'Skills',   comp: SkillsPanel },
  { key: 'general',  label: 'General',  comp: SettingsPanel },
];

function SettingsDialog({ onClose }) {
  const [tab, setTab] = useState('agents');
  const active = DIALOG_TABS.find(t => t.key === tab);

  useEffect(() => {
    document.body.classList.add('dialog-open');
    return () => document.body.classList.remove('dialog-open');
  }, []);

  return h('div', { className: 'dialog-overlay', onClick: (e) => { if (e.target === e.currentTarget) onClose(); } },
    h('div', { className: 'dialog' },
      h('div', { className: 'dialog-header' },
        h('span', { className: 'dialog-title' }, 'Settings'),
        h('button', { className: 'btn btn-invisible btn-sm', onClick: onClose, 'aria-label': 'Close' }, '✕'),
      ),
      h('div', { className: 'dialog-body' },
        h('nav', { className: 'dialog-tabs SideNav' },
          DIALOG_TABS.map(t =>
            h('button', {
              key: t.key,
              className: 'dialog-tab SideNav-item',
              'aria-current': tab === t.key ? 'page' : undefined,
              onClick: () => setTab(t.key),
            }, t.label),
          ),
        ),
        h('div', { className: 'dialog-content' },
          active ? h(active.comp) : null,
        ),
      ),
    ),
  );
}

function LoginPage({ onLogin }) {
  const [token, setTokenVal] = useState('');
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(false);

  const handleSubmit = useCallback(async (e) => {
    e.preventDefault();
    setError('');
    setLoading(true);
    try {
      await login(token);
      onLogin();
    } catch {
      setError('Invalid token');
    } finally {
      setLoading(false);
    }
  }, [token, onLogin]);

  return h('div', { className: 'login-page' },
    h('form', { className: 'login-card', onSubmit: handleSubmit },
      h('div', { className: 'login-icon' },
        h('img', { src: '/icon.svg', width: 56, height: 56 }),
      ),
      h('h1', { className: 'login-title' }, 'Agents Server'),
      h('input', {
        className: 'form-control',
        type: 'password',
        placeholder: 'Enter token',
        value: token,
        autoFocus: true,
        onChange: (e) => setTokenVal(e.target.value),
      }),
      error && h('div', { className: 'login-error' }, error),
      h('button', {
        className: 'btn btn-primary login-btn',
        type: 'submit',
        disabled: loading || !token,
      }, loading ? 'Verifying...' : 'Login'),
    ),
  );
}

function defaultSS() {
  return { messages: [], streaming: '', running: false, traceRuns: {}, liveRunId: null, loaded: false };
}
const DEFAULT_SS = defaultSS();

const MemoizedChatView = React.memo(ChatView);

function buildTimeline(msgs) {
  if (!msgs) return [];
  const timeline = [];
  const pendingTC = {};
  let turn = null;
  const ensureTurn = () => {
    if (!turn) { turn = { role: 'turn', parts: [] }; timeline.push(turn); }
  };
  const finishTurn = () => { turn = null; };
  for (const m of msgs) {
    if (m.role === 'user') {
      finishTurn();
      if (m.content) timeline.push({ role: 'user', content: m.content });
    } else if (m.role === 'tool_call') {
      try {
        const item = JSON.parse(m.item);
        ensureTurn();
        const tc = { tool_call_id: item.call_id, tool_name: item.name, arguments: item.arguments || '', output: null, status: null };
        pendingTC[item.call_id] = tc;
        const last = turn.parts[turn.parts.length - 1];
        if (last && last.type === 'tools') { last.toolCalls.push(tc); }
        else { turn.parts.push({ type: 'tools', toolCalls: [tc] }); }
      } catch (_) {}
    } else if (m.role === 'tool_output') {
      try {
        const item = JSON.parse(m.item);
        if (pendingTC[item.call_id]) {
          pendingTC[item.call_id].output = item.output || m.content;
          pendingTC[item.call_id].status = 'completed';
        }
      } catch (_) {}
    } else if (m.role === 'system' && m.content) {
      finishTurn();
      timeline.push({ role: 'system', content: m.content });
    } else if (m.content) {
      ensureTurn();
      turn.parts.push({ type: 'text', content: m.content });
    }
  }
  finishTurn();
  return timeline;
}

function formatHookDetail(ev) {
  const parts = [];
  if (ev.agent_name) parts.push(ev.agent_name);
  if (ev.tool_name) parts.push('→ ' + ev.tool_name);
  if (ev.from && ev.to) parts.push(ev.from + ' → ' + ev.to);
  if (ev.detail) parts.push(ev.detail);
  return parts.join(' ');
}

function App() {
  const [authed, setAuthed] = useState(!!getToken());
  const [checking, setChecking] = useState(true);
  const [activeSession, setActiveSession] = useState(null);
  const [selectedFile, setSelectedFile] = useState(null);
  const [view, setView] = useState('chat');
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [sessionReloadKey, setSessionReloadKey] = useState(0);
  const [settingsReloadKey, setSettingsReloadKey] = useState(0);

  const [ss, setSS] = useState({});
  const wsRef = useRef(null);
  const runMapRef = useRef({});
  const sessionRunRef = useRef({});
  const streamBufsRef = useRef({});
  const loadedRef = useRef(new Set());

  useEffect(() => {
    checkAuth().then(ok => { setAuthed(ok); setChecking(false); });
  }, []);

  useEffect(() => {
    const handler = () => setAuthed(false);
    window.addEventListener('auth:logout', handler);
    return () => window.removeEventListener('auth:logout', handler);
  }, []);

  const updateSS = useCallback((sid, fn) => {
    setSS(prev => {
      const cur = prev[sid] || defaultSS();
      const next = fn(cur);
      return next === cur ? prev : { ...prev, [sid]: next };
    });
  }, []);

  useEffect(() => {
    if (!activeSession || loadedRef.current.has(activeSession)) return;
    loadedRef.current.add(activeSession);
    api.sessions.messages(activeSession).then(msgs => {
      const timeline = buildTimeline(msgs);
      updateSS(activeSession, s => s.messages.length > 0 ? s : { ...s, messages: timeline, loaded: true });
    });
    api.sessions.traces(activeSession).then(events => {
      if (!events || events.length === 0) return;
      const runs = {};
      for (const ev of events) {
        const rid = ev.run_id || 'unknown';
        if (!runs[rid]) runs[rid] = [];
        runs[rid].push(ev);
      }
      updateSS(activeSession, s => Object.keys(s.traceRuns).length > 0 ? s : { ...s, traceRuns: runs });
    }).catch(() => {});
  }, [activeSession, updateSS]);

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
        const msgs = [...s.messages];
        for (let i = msgs.length - 1; i >= 0; i--) {
          if (msgs[i].role !== 'turn') continue;
          const parts = [...msgs[i].parts];
          for (let j = parts.length - 1; j >= 0; j--) {
            if (parts[j].type !== 'tools') continue;
            const tcs = parts[j].toolCalls;
            const idx = tcs.findIndex(tc => tc.tool_call_id === p.tool_call_id || (!p.tool_call_id && !tc.output));
            if (idx >= 0) {
              const newTcs = [...tcs];
              newTcs[idx] = { ...newTcs[idx], output: p.output, status: 'completed' };
              parts[j] = { ...parts[j], toolCalls: newTcs };
              msgs[i] = { ...msgs[i], parts };
              return { ...s, messages: msgs };
            }
          }
        }
        return s;
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

    ws.on('session.title_updated', () => {
      setSessionReloadKey(k => k + 1);
    });

    ws.connect();
    return () => ws.close();
  }, [updateSS]);

  const handleSend = useCallback((text, agentConfigId, sandboxId) => {
    if (!activeSession || !wsRef.current) return;
    updateSS(activeSession, s => ({ ...s, messages: [...s.messages, { role: 'user', content: text }] }));
    const payload = { session_id: activeSession, input: text, agent_config_id: agentConfigId };
    if (sandboxId) payload.sandbox_id = sandboxId;
    wsRef.current.send('run.create', payload);
  }, [activeSession, updateSS]);

  const handleCancel = useCallback(() => {
    if (!wsRef.current || !activeSession) return;
    const runId = sessionRunRef.current[activeSession];
    if (!runId) return;
    wsRef.current.send('run.cancel', { run_id: runId });
  }, [activeSession]);

  const updateToolCall = useCallback((toolCallId, patch) => {
    if (!activeSession) return;
    updateSS(activeSession, s => {
      const msgs = [...s.messages];
      for (let i = msgs.length - 1; i >= 0; i--) {
        if (msgs[i].role !== 'turn') continue;
        const parts = [...msgs[i].parts];
        for (let j = parts.length - 1; j >= 0; j--) {
          if (parts[j].type !== 'tools') continue;
          const tcs = parts[j].toolCalls;
          const idx = tcs.findIndex(tc => tc.tool_call_id === toolCallId);
          if (idx >= 0) {
            const newTcs = [...tcs];
            newTcs[idx] = { ...newTcs[idx], ...patch };
            parts[j] = { ...parts[j], toolCalls: newTcs };
            msgs[i] = { ...msgs[i], parts };
            return { ...s, messages: msgs };
          }
        }
      }
      return s;
    });
  }, [activeSession, updateSS]);

  const handleApprove = useCallback((toolCallId) => {
    if (!wsRef.current) return;
    updateToolCall(toolCallId, { status: 'approved' });
    wsRef.current.send('tool.approve', { tool_call_id: toolCallId });
  }, [updateToolCall]);

  const handleReject = useCallback((toolCallId) => {
    if (!wsRef.current) return;
    updateToolCall(toolCallId, { status: 'rejected' });
    wsRef.current.send('tool.reject', { tool_call_id: toolCallId });
  }, [updateToolCall]);

  const runningSessions = useMemo(() => {
    const set = new Set();
    for (const [sid, state] of Object.entries(ss)) {
      if (state.running) set.add(sid);
    }
    return set;
  }, [ss]);

  if (checking) return h(ThemeProvider, null, null);
  if (!authed) return h(ThemeProvider, null, h(LoginPage, { onLogin: () => setAuthed(true) }));

  const currentSS = ss[activeSession] || DEFAULT_SS;

  const sessionPane = h(SessionList, {
    activeId: activeSession,
    onSelect: setActiveSession,
    reloadKey: sessionReloadKey,
    runningSessions,
  });

  const filePane = h(FileTree, {
    selectedPath: selectedFile,
    onSelect: setSelectedFile,
  });

  const sidebarPane = view === 'chat'
    ? h('div', { key: 'sidebar-chat', style: { display: 'flex', flexDirection: 'column', height: '100%' } }, sessionPane)
    : view === 'files'
      ? h('div', { key: 'sidebar-files', style: { display: 'flex', flexDirection: 'column', height: '100%' } }, filePane)
      : null;

  let main;
  if (view === 'chat') {
    main = h(MemoizedChatView, {
      sessionId: activeSession,
      messages: currentSS.messages,
      streaming: currentSS.streaming,
      running: currentSS.running,
      traceRuns: currentSS.traceRuns,
      liveRunId: currentSS.liveRunId,
      lastError: currentSS.lastError,
      onSend: handleSend,
      onCancel: handleCancel,
      onApprove: handleApprove,
      onReject: handleReject,
      settingsReloadKey,
    });
  } else if (view === 'files') {
    main = h(FileViewer, { filePath: selectedFile });
  }

  return h(ThemeProvider, null,
    h(AppShell, { view, onViewChange: setView, onSettingsOpen: () => setSettingsOpen(true), sidebarPane }, main),
    settingsOpen && h(SettingsDialog, { onClose: () => { setSettingsOpen(false); setSettingsReloadKey(k => k + 1); } }),
  );
}

const root = ReactDOM.createRoot(document.getElementById('root'));
root.render(h(App));
