import React from 'react';
import ReactDOM from 'react-dom/client';
import { ThemeProvider } from '/theme/ThemeProvider.jsx';
import { AppShell } from '/layout/AppShell.jsx';
import { SessionList } from '/features/sessions/SessionList.jsx';
import { ChatView } from '/features/chat/ChatView.jsx';
import { FileTree } from '/features/files/FileTree.jsx';
import { FileViewer } from '/features/files/FileViewer.jsx';
import { login, checkAuth, getToken, api } from '/lib/api.js';
import { useAgentSocket } from '/lib/useAgentSocket.js';
import { patchToolCall } from '/lib/timeline.js';

const { useState, useCallback, useEffect, useRef, useMemo } = React;
const h = React.createElement;

const DIALOG_TABS = [
  { key: 'agents',     label: 'Agents',     load: () => import('/features/agents/AgentConfigPanel.jsx') },
  { key: 'mcp',        label: 'MCP',        load: () => import('/features/mcp/McpServerPanel.jsx') },
  { key: 'guardrails', label: 'Guardrails', load: () => import('/features/guardrails/GuardrailPanel.jsx') },
  { key: 'skills',     label: 'Skills',     load: () => import('/features/skills/SkillsPanel.jsx') },
  { key: 'sandbox',    label: 'Sandbox',    load: () => import('/features/sandbox/SandboxPanel.jsx') },
  { key: 'memory',     label: 'Memory',     load: () => import('/features/memory/MemoryPanel.jsx') },
  { key: 'general',    label: 'General',    load: () => import('/features/settings/SettingsPanel.jsx') },
];

const EXPORT_MAP = {
  agents: 'AgentConfigPanel', mcp: 'McpServerPanel', guardrails: 'GuardrailPanel',
  skills: 'SkillsPanel', sandbox: 'SandboxPanel', memory: 'MemoryPanel', general: 'SettingsPanel',
};

function SettingsDialog({ onClose }) {
  const [tab, setTab] = useState('agents');
  const [TabComp, setTabComp] = useState(null);

  useEffect(() => {
    document.body.classList.add('dialog-open');
    return () => document.body.classList.remove('dialog-open');
  }, []);

  useEffect(() => {
    setTabComp(null);
    const entry = DIALOG_TABS.find(t => t.key === tab);
    if (!entry) return;
    entry.load().then(mod => {
      setTabComp(() => mod[EXPORT_MAP[tab]]);
    });
  }, [tab]);

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
          TabComp ? h(TabComp) : null,
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

  const { wsRef, sessionRunRef, loadSession, deleteSession } = useAgentSocket(updateSS);

  useEffect(() => {
    if (activeSession) loadSession(activeSession);
  }, [activeSession, loadSession]);

  useEffect(() => {
    if (!wsRef.current) return;
    wsRef.current.on('session.title_updated', () => {
      setSessionReloadKey(k => k + 1);
    });
  }, [wsRef]);

  const handleSend = useCallback((text, agentConfigId, sandboxId) => {
    if (!activeSession || !wsRef.current) return;
    if (!wsRef.current.isConnected()) {
      updateSS(activeSession, s => ({ ...s, lastError: 'WebSocket disconnected — message not sent' }));
      return;
    }
    updateSS(activeSession, s => ({ ...s, messages: [...s.messages, { role: 'user', content: text }] }));
    const payload = { session_id: activeSession, input: text, agent_config_id: agentConfigId };
    if (sandboxId) payload.sandbox_id = sandboxId;
    wsRef.current.send('run.create', payload);
  }, [activeSession, updateSS, wsRef]);

  const handleCancel = useCallback(() => {
    if (!wsRef.current || !activeSession) return;
    const runId = sessionRunRef.current[activeSession];
    if (!runId) return;
    wsRef.current.send('run.cancel', { run_id: runId });
  }, [activeSession, wsRef, sessionRunRef]);

  const updateToolCall = useCallback((toolCallId, patch) => {
    if (!activeSession) return;
    updateSS(activeSession, s => {
      const patched = patchToolCall(s.messages, toolCallId, patch);
      return patched ? { ...s, messages: patched } : s;
    });
  }, [activeSession, updateSS]);

  const handleApprove = useCallback((toolCallId) => {
    if (!wsRef.current) return;
    updateToolCall(toolCallId, { status: 'approved' });
    wsRef.current.send('tool.approve', { tool_call_id: toolCallId });
  }, [updateToolCall, wsRef]);

  const handleReject = useCallback((toolCallId) => {
    if (!wsRef.current) return;
    updateToolCall(toolCallId, { status: 'rejected' });
    wsRef.current.send('tool.reject', { tool_call_id: toolCallId });
  }, [updateToolCall, wsRef]);

  const handleDeleteSession = useCallback((deletedId) => {
    deleteSession(deletedId);
    setSS(prev => {
      if (!prev[deletedId]) return prev;
      const next = { ...prev };
      delete next[deletedId];
      return next;
    });
  }, [deleteSession]);

  const handleFork = useCallback(async (messageId) => {
    if (!activeSession) return;
    const forked = await api.sessions.fork(activeSession, messageId);
    setSessionReloadKey(k => k + 1);
    setActiveSession(forked.id);
  }, [activeSession]);

  const runningSessions = useMemo(() => {
    const set = new Set();
    for (const [sid, state] of Object.entries(ss)) {
      if (state.running) set.add(sid);
    }
    return set;
  }, [ss]);

  const handleSessionCreated = useCallback(() => {
    setTimeout(() => {
      const el = document.querySelector('.chat-input-box textarea');
      if (el) el.focus();
    }, 0);
  }, []);

  if (checking) return h(ThemeProvider, null, null);
  if (!authed) return h(ThemeProvider, null, h(LoginPage, { onLogin: () => setAuthed(true) }));

  const currentSS = ss[activeSession] || DEFAULT_SS;

  const sessionPane = h(SessionList, {
    activeId: activeSession,
    onSelect: setActiveSession,
    onDelete: handleDeleteSession,
    onCreated: handleSessionCreated,
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
      loaded: currentSS.loaded,
      streaming: currentSS.streaming,
      running: currentSS.running,
      traceRuns: currentSS.traceRuns,
      liveRunId: currentSS.liveRunId,
      lastError: currentSS.lastError,
      onSend: handleSend,
      onCancel: handleCancel,
      onApprove: handleApprove,
      onReject: handleReject,
      onFork: handleFork,
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
