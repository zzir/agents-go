import React, { useState, useCallback, useEffect, useRef, useMemo, memo } from 'react';
import ReactDOM from 'react-dom/client';
import { TextInput, Dialog, NavList as PrimerNavList, Flash } from '@primer/react';
import {
  DependabotIcon, McpIcon, ShieldCheckIcon, ZapIcon,
  ContainerIcon, DatabaseIcon, GearIcon,
  XCircleFillIcon, AlertFillIcon, CheckCircleFillIcon, InfoIcon,
} from '@primer/octicons-react';
import type { Icon } from '@primer/octicons-react';
import { ThemeProvider } from '@/theme/ThemeProvider';
import { AppShell } from '@/layout/AppShell';
import { SessionList } from '@/features/sessions/SessionList';
import { ChatView } from '@/features/chat/ChatView';
import { login, checkAuth, getToken, api } from '@/lib/api';
import { useAgentSocket, defaultSS, type SessionState } from '@/lib/useAgentSocket';
import { patchToolCall } from '@/lib/timeline';
import { onToast, toast } from '@/lib/toast';

const FLASH_VARIANT: Record<string, FlashProps['variant']> = { error: 'danger', warning: 'warning', success: 'success', info: 'default' };
const FLASH_ICON: Record<string, React.ReactNode> = {
  error: <XCircleFillIcon size={16} />,
  warning: <AlertFillIcon size={16} />,
  success: <CheckCircleFillIcon size={16} />,
  info: <InfoIcon size={16} />,
};
type FlashProps = React.ComponentProps<typeof Flash>;

function GlobalToast() {
  const [item, setItem] = useState<{ msg: string; type: string; seq: number } | null>(null);
  const [exiting, setExiting] = useState(false);
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const exitTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const seqRef = useRef(0);

  const dismiss = useCallback(() => {
    setExiting(true);
    exitTimerRef.current = setTimeout(() => { setItem(null); setExiting(false); exitTimerRef.current = null; }, 150);
  }, []);

  useEffect(() => {
    onToast(({ msg, type }) => {
      if (timerRef.current) clearTimeout(timerRef.current);
      if (exitTimerRef.current) { clearTimeout(exitTimerRef.current); exitTimerRef.current = null; }
      setExiting(false);
      seqRef.current += 1;
      setItem({ msg, type, seq: seqRef.current });
      timerRef.current = setTimeout(() => { dismiss(); timerRef.current = null; }, 4000);
    });
    return () => onToast(null);
  }, [dismiss]);

  if (!item) return null;
  return (
    <Flash
      key={item.seq}
      variant={FLASH_VARIANT[item.type] || 'default'}
      className={'global-toast' + (exiting ? ' global-toast-exit' : '')}
      onClick={() => { if (timerRef.current) clearTimeout(timerRef.current); dismiss(); }}
    >
      <span style={{ display: 'inline-flex', alignItems: 'center', gap: 8 }}>
        {FLASH_ICON[item.type]}{item.msg}
      </span>
    </Flash>
  );
}

const DIALOG_TABS: { key: string; label: string; icon: Icon; load: () => Promise<{ default: React.ComponentType }> }[] = [
  { key: 'agents',     label: 'Agents',     icon: DependabotIcon, load: () => import('@/features/agents/AgentConfigPanel') },
  { key: 'mcp',        label: 'MCP',        icon: McpIcon,        load: () => import('@/features/mcp/McpServerPanel') },
  { key: 'guardrails', label: 'Guardrails', icon: ShieldCheckIcon, load: () => import('@/features/guardrails/GuardrailPanel') },
  { key: 'skills',     label: 'Skills',     icon: ZapIcon,        load: () => import('@/features/skills/SkillsPanel') },
  { key: 'sandbox',    label: 'Sandbox',    icon: ContainerIcon,  load: () => import('@/features/sandbox/SandboxPanel') },
  { key: 'memory',     label: 'Memory',     icon: DatabaseIcon,   load: () => import('@/features/memory/MemoryPanel') },
  { key: 'general',    label: 'General',    icon: GearIcon,       load: () => import('@/features/settings/SettingsPanel') },
];

function SettingsDialog({ onClose }: { onClose: () => void }) {
  const [tab, setTab] = useState('agents');
  const [TabComp, setTabComp] = useState<React.ComponentType | null>(null);

  useEffect(() => {
    setTabComp(null);
    const entry = DIALOG_TABS.find(t => t.key === tab);
    if (!entry) return;
    entry.load().then(mod => {
      setTabComp(() => mod.default);
    });
  }, [tab]);

  return (
    <Dialog
      title="Settings"
      onClose={() => onClose()}
      height="large"
      style={{ width: 'min(960px, calc(100vw - 64px))' }}
      renderBody={({ children }) => (
        <Dialog.Body className="settings-body" style={{ padding: 0 }}>
          {children}
        </Dialog.Body>
      )}
    >
      <div className="settings-layout">
        <nav className="settings-nav">
          <PrimerNavList aria-label="Settings sections">
            {DIALOG_TABS.map(t => (
              <PrimerNavList.Item
                key={t.key}
                aria-current={tab === t.key ? 'page' : undefined}
                onClick={() => setTab(t.key)}
              >
                <PrimerNavList.LeadingVisual><t.icon size={16} /></PrimerNavList.LeadingVisual>
                {t.label}
              </PrimerNavList.Item>
            ))}
          </PrimerNavList>
        </nav>
        <div className="settings-content">
          {TabComp ? <TabComp /> : null}
        </div>
      </div>
    </Dialog>
  );
}

function LoginPage({ onLogin }: { onLogin: () => void }) {
  const [token, setTokenVal] = useState('');
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(false);

  const handleSubmit = useCallback(async (e: React.FormEvent) => {
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

  return (
    <div className="login-page">
      <form className="login-card" onSubmit={handleSubmit}>
        <img src="/icon.svg" width={48} height={48} />
        <TextInput
          type="password"
          placeholder="Token"
          value={token}
          autoFocus
          loading={loading || undefined}
          onChange={(e) => setTokenVal(e.target.value)}
          validationStatus={error ? 'error' : undefined}
        />
      </form>
    </div>
  );
}

const DEFAULT_SS = defaultSS();

const MemoizedChatView = memo(ChatView);

function readHashSession(): string | null {
  const h = window.location.hash;
  const m = /^#\/session\/([a-zA-Z0-9_-]+)$/.exec(h);
  return m ? m[1] : null;
}

function writeHashSession(id: string | null) {
  const next = id ? `#/session/${id}` : '';
  if (window.location.hash !== next) {
    window.history.replaceState(null, '', next || window.location.pathname);
  }
}

function App() {
  const [authed, setAuthed] = useState(!!getToken());
  const [checking, setChecking] = useState(true);
  const [activeSession, setActiveSession] = useState<string | null>(readHashSession);
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [sidebarOpen, setSidebarOpen] = useState(false);
  const [sessionReloadKey, setSessionReloadKey] = useState(0);
  const [settingsReloadKey, setSettingsReloadKey] = useState(0);

  const [ss, setSS] = useState<Record<string, SessionState>>({});

  useEffect(() => {
    checkAuth().then(ok => { setAuthed(ok); setChecking(false); });
  }, []);

  useEffect(() => {
    writeHashSession(activeSession);
  }, [activeSession]);

  useEffect(() => {
    const onHash = () => {
      const id = readHashSession();
      setActiveSession(prev => prev === id ? prev : id);
    };
    window.addEventListener('hashchange', onHash);
    return () => window.removeEventListener('hashchange', onHash);
  }, []);

  useEffect(() => {
    const handler = () => setAuthed(false);
    window.addEventListener('auth:logout', handler);
    return () => window.removeEventListener('auth:logout', handler);
  }, []);

  const updateSS = useCallback((sid: string, fn: (s: SessionState) => SessionState) => {
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

  const handleSend = useCallback((text: string, agentConfigId?: string, sandboxId?: string) => {
    if (!activeSession || !wsRef.current) return;
    if (!wsRef.current.isConnected()) {
      toast.error('WebSocket disconnected — message not sent');
      return;
    }
    updateSS(activeSession, s => ({ ...s, messages: [...s.messages, { role: 'user', content: text }] }));
    const payload: Record<string, any> = { session_id: activeSession, input: text, agent_config_id: agentConfigId };
    if (sandboxId) payload.sandbox_id = sandboxId;
    wsRef.current.send('run.create', payload);
  }, [activeSession, updateSS, wsRef]);

  const handleCancel = useCallback((graceful?: boolean) => {
    if (!wsRef.current || !activeSession) return;
    const runId = sessionRunRef.current[activeSession];
    if (!runId) return;
    wsRef.current.send('run.cancel', { run_id: runId, mode: graceful ? 'graceful' : '' });
  }, [activeSession, wsRef, sessionRunRef]);

  const updateToolCall = useCallback((toolCallId: string, patch: Record<string, any>) => {
    if (!activeSession) return;
    updateSS(activeSession, s => {
      const patched = patchToolCall(s.messages, toolCallId, patch);
      return patched ? { ...s, messages: patched } : s;
    });
  }, [activeSession, updateSS]);

  const handleApprove = useCallback((toolCallId: string, scope?: string) => {
    if (!wsRef.current) return;
    updateToolCall(toolCallId, { status: 'approved' });
    if (!wsRef.current.send('tool.approve', { tool_call_id: toolCallId, scope })) {
      // The socket is down: undo the optimistic status so the card stays
      // actionable — a silently dropped approval would strand the paused run.
      updateToolCall(toolCallId, { status: null });
      toast.error('Not connected — approval not sent, try again');
    }
  }, [updateToolCall, wsRef]);

  const handleReject = useCallback((toolCallId: string) => {
    if (!wsRef.current) return;
    updateToolCall(toolCallId, { status: 'rejected' });
    if (!wsRef.current.send('tool.reject', { tool_call_id: toolCallId })) {
      updateToolCall(toolCallId, { status: null });
      toast.error('Not connected — rejection not sent, try again');
    }
  }, [updateToolCall, wsRef]);

  const handleDeleteSession = useCallback((deletedId: string) => {
    deleteSession(deletedId);
    setSS(prev => {
      if (!prev[deletedId]) return prev;
      const next = { ...prev };
      delete next[deletedId];
      return next;
    });
  }, [deleteSession]);

  const handleFork = useCallback(async (messageId: string | number) => {
    if (!activeSession) return;
    const forked = await api.sessions.fork(activeSession, Number(messageId));
    setSessionReloadKey(k => k + 1);
    setActiveSession(forked.id);
  }, [activeSession]);

  const handleRegenerate = useCallback(async (userMessageId: string | number, userContent: string, agentConfigId: string, sandboxId: string) => {
    if (!activeSession || !wsRef.current) return;
    try {
      const forked = await api.sessions.fork(activeSession, Number(userMessageId), { exclusive: true, label: 'regen' });
      setSessionReloadKey(k => k + 1);
      setActiveSession(forked.id);
      await loadSession(forked.id);
      updateSS(forked.id, s => ({ ...s, messages: [...s.messages, { role: 'user', content: userContent }] }));
      const payload: Record<string, any> = { session_id: forked.id, input: userContent, agent_config_id: agentConfigId };
      if (sandboxId) payload.sandbox_id = sandboxId;
      wsRef.current.send('run.create', payload);
    } catch (e: any) {
      toast.error(e.message || 'Regenerate failed');
    }
  }, [activeSession, wsRef, updateSS, loadSession]);

  const runningSessions = useMemo(() => {
    const set = new Set<string>();
    for (const [sid, state] of Object.entries(ss)) {
      if (state.running) set.add(sid);
    }
    return set;
  }, [ss]);

  // A session is awaiting approval when its latest turn holds a tool call that
  // needs approval and has no decision yet. Derived from the messages (not a
  // transient socket flag), so it survives a reload — the paused turn is rebuilt
  // from the durable approvals — and self-clears the moment approve/reject sets
  // a status.
  const awaitingSessions = useMemo(() => {
    const set = new Set<string>();
    for (const [sid, state] of Object.entries(ss)) {
      for (const m of state.messages) {
        if (m.role !== 'turn') continue;
        for (const part of (m as { parts?: Array<{ type: string; toolCalls?: Array<{ needs_approval?: boolean; status?: string | null }> }> }).parts || []) {
          if (part.type !== 'tools') continue;
          if ((part.toolCalls || []).some(tc => tc.needs_approval && !tc.status)) { set.add(sid); break; }
        }
        if (set.has(sid)) break;
      }
    }
    return set;
  }, [ss]);

  const handleSessionCreated = useCallback(() => {
    setTimeout(() => {
      const el = document.querySelector('.chat-input-box textarea') as HTMLTextAreaElement | null;
      if (el) el.focus();
    }, 0);
  }, []);

  const handleSelectSession = useCallback((id: string | null) => {
    setActiveSession(id);
    if (window.innerWidth < 768) setSidebarOpen(false);
  }, []);

  if (!authed && !checking) return <ThemeProvider><LoginPage onLogin={() => setAuthed(true)} /></ThemeProvider>;
  if (!authed) return <ThemeProvider>{null}</ThemeProvider>;

  const currentSS = ss[activeSession!] || DEFAULT_SS;

  const sidebarPane = (
    <SessionList
      activeId={activeSession}
      onSelect={handleSelectSession}
      onDelete={handleDeleteSession}
      onCreated={handleSessionCreated}
      reloadKey={sessionReloadKey}
      runningSessions={runningSessions}
      awaitingSessions={awaitingSessions}
    />
  );

  const main = (
    <MemoizedChatView
      sessionId={activeSession}
      messages={currentSS.messages}
      loaded={currentSS.loaded}
      streaming={currentSS.streaming}
      reasoning={currentSS.reasoning}
      running={currentSS.running}
      compacting={currentSS.compacting}
      traceRuns={currentSS.traceRuns}
      liveRunId={currentSS.liveRunId}
      liveStartedAt={currentSS.liveStartedAt}
      liveAgentName={currentSS.liveAgentName}
      onSend={handleSend}
      onCancel={handleCancel}
      onApprove={handleApprove}
      onReject={handleReject}
      onFork={handleFork}
      onRegenerate={handleRegenerate}
      settingsReloadKey={settingsReloadKey}
    />
  );

  return (
    <ThemeProvider>
      <AppShell onSettingsOpen={() => setSettingsOpen(true)} sidebarPane={sidebarPane} sidebarOpen={sidebarOpen} onSidebarToggle={setSidebarOpen}>
        {main}
      </AppShell>
      {settingsOpen && <SettingsDialog onClose={() => { setSettingsOpen(false); setSettingsReloadKey(k => k + 1); }} />}
      <GlobalToast />
    </ThemeProvider>
  );
}

const root = ReactDOM.createRoot(document.getElementById('root')!);
root.render(<App />);
