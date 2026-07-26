import React, { useState, useCallback, useEffect, useRef, useMemo, memo } from 'react';
import { TextInput, Dialog, NavList as PrimerNavList, Flash, Button } from '@primer/react';
import {
  DependabotIcon, McpIcon, ShieldCheckIcon, ZapIcon,
  ContainerIcon, DatabaseIcon, GearIcon,
  XCircleFillIcon, AlertFillIcon, CheckCircleFillIcon, InfoIcon,
} from '@primer/octicons-react';
import type { Icon } from '@primer/octicons-react';
import { ThemeProvider } from '@/theme/ThemeProvider';
import { AppShell } from '@/layout/AppShell';
import { SessionList } from '@/features/sessions/SessionList';
import { ChatView, type InspectorPanel } from '@/features/chat/ChatView';

// Lazy: xterm (+ webgl renderer) is a few hundred KB the first paint never
// needs — the chunk loads when the terminal panel first opens, then the panel
// stays mounted (hidden) so its sessions survive toggles.
const TerminalPanel = React.lazy(() =>
  import('@/features/terminal/TerminalPanel').then(m => ({ default: m.TerminalPanel })),
);
import { login, checkAuth, getToken, api } from '@/lib/api';
import { EV } from '@/lib/protocol';
import { useAgentSocket, defaultSS, type SessionState } from '@/lib/useAgentSocket';
import { patchToolCall } from '@/lib/timeline';
import { clearSessionPrefs } from '@/lib/drafts';
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

// Monotonic client-side id stamped on each optimistic user bubble. It lets the
// socket layer roll back a specific un-sent message (on session_busy or a
// dropped send) and lets the stream reducer dedup two identical-text sends
// without collapsing them into one.
let clientMsgSeq = 0;
function nextClientMsgId(): string { return 'c' + (++clientMsgSeq); }

const MemoizedChatView = memo(ChatView);

function panelKey(p: InspectorPanel): string {
  if (!p) return '';
  if (p.kind === 'task') return `task/${p.taskId}`;
  return p.kind;
}

function readHash(): { sessionId: string | null; panel: InspectorPanel } {
  const h = window.location.hash;
  const m = /^#\/session\/([a-zA-Z0-9_-]+)(?:\/(trace|tasks|task\/([a-zA-Z0-9_-]+)))?$/.exec(h);
  if (!m) return { sessionId: null, panel: null };
  let panel: InspectorPanel = null;
  if (m[2] === 'trace') panel = { kind: 'trace' };
  else if (m[2] === 'tasks') panel = { kind: 'tasks' };
  else if (m[3]) panel = { kind: 'task', taskId: m[3] };
  return { sessionId: m[1], panel };
}

function writeHash(sessionId: string | null, panel: InspectorPanel) {
  let next = '';
  if (sessionId) {
    next = `#/session/${sessionId}`;
    if (panel?.kind === 'trace') next += '/trace';
    else if (panel?.kind === 'tasks') next += '/tasks';
    else if (panel?.kind === 'task') next += `/task/${panel.taskId}`;
  }
  if (window.location.hash !== next) {
    window.history.replaceState(null, '', next || window.location.pathname);
  }
}

export default function App() {
  const [authed, setAuthed] = useState(!!getToken());
  const [checking, setChecking] = useState(true);
  // The initial auth check failed at the network level (server unreachable), as
  // opposed to resolving "not authenticated". Without this the app would sit on
  // a blank screen forever; instead we surface a retryable error state.
  const [checkError, setCheckError] = useState(false);
  const [activeSession, setActiveSession] = useState<string | null>(() => readHash().sessionId);
  const [activePanel, setActivePanel] = useState<InspectorPanel>(() => readHash().panel);
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [sidebarOpen, setSidebarOpen] = useState(false);
  const [sessionReloadKey, setSessionReloadKey] = useState(0);
  const [settingsReloadKey, setSettingsReloadKey] = useState(0);
  // Global terminal panel: session-agnostic, opened from the composer (the
  // button only ever opens; closing/collapsing lives on the panel itself).
  // everOpened defers mounting (and the xterm chunk) until first use, after
  // which the panel stays mounted while hidden to keep sessions alive.
  const [terminalOpen, setTerminalOpen] = useState(false);
  const [terminalEverOpened, setTerminalEverOpened] = useState(false);
  // A one-shot "start a terminal for this sandbox" request, set when the
  // composer button opens a CLOSED panel with a capable sandbox selected (an
  // already-open panel is left alone). The nonce distinguishes repeat requests
  // for the same sandbox.
  const [terminalRequest, setTerminalRequest] = useState<{ id: string; name: string; nonce: number } | null>(null);
  const terminalOpenRef = useRef(false);
  terminalOpenRef.current = terminalOpen;
  const terminalNonceRef = useRef(0);
  const handleTerminalOpen = useCallback((sandbox?: { id: string; name: string }) => {
    if (!terminalOpenRef.current && sandbox) {
      setTerminalRequest({ ...sandbox, nonce: ++terminalNonceRef.current });
    }
    setTerminalOpen(true);
    setTerminalEverOpened(true);
  }, []);

  const [ss, setSS] = useState<Record<string, SessionState>>({});

  const runCheck = useCallback(() => {
    setChecking(true);
    setCheckError(false);
    checkAuth()
      .then(ok => { setAuthed(ok); setChecking(false); })
      // A network-level failure (server down, offline) rejects here — don't
      // stay stuck in "checking"; show the retry screen below.
      .catch(() => { setChecking(false); setCheckError(true); });
  }, []);

  useEffect(() => { runCheck(); }, [runCheck]);

  useEffect(() => {
    writeHash(activeSession, activePanel);
  }, [activeSession, activePanel]);

  useEffect(() => {
    const onHash = () => {
      const { sessionId, panel } = readHash();
      setActiveSession(prev => prev === sessionId ? prev : sessionId);
      setActivePanel(prev => panelKey(prev) === panelKey(panel) ? prev : panel);
    };
    window.addEventListener('hashchange', onHash);
    return () => window.removeEventListener('hashchange', onHash);
  }, []);

  useEffect(() => {
    // A logout is a definitive "not authenticated" — clear any lingering
    // network-error state so the login page shows, not the retry screen.
    const handler = () => { setAuthed(false); setCheckError(false); };
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

  const { wsRef, sessionRunRef, loadSession, deleteSession, loadEarlier, forgetLoaded, watchTask, unwatchTask } = useAgentSocket(updateSS);

  // patchTask applies a server-confirmed task state change (e.g. the stop
  // API's response) directly — the fallback for when no hub broadcast will
  // come (stopping a paused task after a restart).
  const patchTask = useCallback((sid: string, taskId: string, patch: Record<string, unknown>) => {
    updateSS(sid, s => s.tasks[taskId]
      ? { ...s, tasks: { ...s.tasks, [taskId]: { ...s.tasks[taskId], ...patch } } }
      : s);
  }, [updateSS]);

  useEffect(() => {
    if (!activeSession) return;
    let cancelled = false;
    // The session id can come from the URL hash and may not exist (stale link,
    // deleted session, hand-typed id). The messages endpoint returns [] for an
    // unknown session rather than 404, so validate existence explicitly: a 404
    // means drop the id — the app falls back to the empty state and typing then
    // starts a new chat instead of running against a non-existent session.
    const tryLoad = () => loadSession(activeSession).catch(() => toast.error('Could not load conversation'));
    api.sessions.get(activeSession)
      .then(() => { if (!cancelled) tryLoad(); })
      .catch((e: { status?: number }) => {
        if (cancelled) return;
        if (e?.status === 404) setActiveSession(null);
        else tryLoad(); // transient error — try loading anyway
      });
    return () => { cancelled = true; };
  }, [activeSession, loadSession]);

  useEffect(() => {
    if (!wsRef.current) return;
    wsRef.current.on(EV.sessionTitleUpdated, () => {
      setSessionReloadKey(k => k + 1);
    });
  }, [wsRef]);

  const handleSend = useCallback(async (text: string, agentConfigId?: string, sandboxId?: string) => {
    if (!wsRef.current) return;
    if (!wsRef.current.isConnected()) {
      toast.error('WebSocket disconnected — message not sent');
      return;
    }
    // Typing straight into the box with no active session starts a new chat,
    // instead of silently dropping the message. The freshly-created session has
    // no history, so mark it loaded to protect the optimistic message from the
    // load-session effect.
    let sid = activeSession;
    let isNew = false;
    if (!sid) {
      try {
        const sess = await api.sessions.create('New Chat', agentConfigId) as { id: string };
        sid = sess.id;
        isNew = true;
        setActiveSession(sid);
        setActivePanel(null);
        setSessionReloadKey(k => k + 1);
      } catch {
        toast.error('Could not start a new chat');
        return;
      }
    }
    const clientMsgId = nextClientMsgId();
    updateSS(sid, s => ({ ...s, messages: [...s.messages, { role: 'user', content: text, clientMsgId }], ...(isNew ? { loaded: true } : {}) }));
    const payload: Record<string, any> = { session_id: sid, input: text, agent_config_id: agentConfigId };
    if (sandboxId) payload.sandbox_id = sandboxId;
    if (!wsRef.current.send(EV.runCreate, payload)) {
      // The socket dropped between the isConnected() check and the send: roll
      // back the optimistic bubble so it isn't left stranded with no run.
      updateSS(sid, s => ({ ...s, messages: s.messages.filter((m: { clientMsgId?: string }) => m.clientMsgId !== clientMsgId) }));
      toast.error('WebSocket disconnected — message not sent');
    }
  }, [activeSession, updateSS, wsRef]);

  const handleCancel = useCallback((graceful?: boolean) => {
    if (!wsRef.current || !activeSession) return;
    const runId = sessionRunRef.current[activeSession];
    if (!runId) return;
    wsRef.current.send(EV.runCancel, { run_id: runId, mode: graceful ? 'graceful' : '' });
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
    if (!wsRef.current.send(EV.toolApprove, { tool_call_id: toolCallId, scope })) {
      // The socket is down: undo the optimistic status so the card stays
      // actionable — a silently dropped approval would strand the paused run.
      updateToolCall(toolCallId, { status: null });
      toast.error('Not connected — approval not sent, try again');
    }
  }, [updateToolCall, wsRef]);

  const handleReject = useCallback((toolCallId: string) => {
    if (!wsRef.current) return;
    updateToolCall(toolCallId, { status: 'rejected' });
    if (!wsRef.current.send(EV.toolReject, { tool_call_id: toolCallId })) {
      updateToolCall(toolCallId, { status: null });
      toast.error('Not connected — rejection not sent, try again');
    }
  }, [updateToolCall, wsRef]);

  const handleDeleteSession = useCallback((deletedId: string) => {
    deleteSession(deletedId);
    clearSessionPrefs(deletedId);
    setSS(prev => {
      if (!prev[deletedId]) return prev;
      const next = { ...prev };
      delete next[deletedId];
      return next;
    });
  }, [deleteSession]);

  const handleLoadEarlier = useCallback(() => {
    if (activeSession) loadEarlier(activeSession);
  }, [activeSession, loadEarlier]);

  const handleFork = useCallback(async (messageId: string | number) => {
    if (!activeSession) return;
    try {
      const forked = await api.sessions.fork(activeSession, Number(messageId));
      setSessionReloadKey(k => k + 1);
      setActiveSession(forked.id);
      setActivePanel(null);
    } catch (e) {
      toast.error((e as Error).message || 'Fork failed');
    }
  }, [activeSession]);

  // reloadTimeline re-reads a session's persisted history after a branch move.
  // The switch is a server-side append, so the client's assembled timeline is
  // stale in a way no local patch can fix — a different branch is a different
  // conversation.
  const reloadTimeline = useCallback(async (sid: string) => {
    forgetLoaded(sid);
    await loadSession(sid).catch(() => toast.error('Could not reload conversation'));
  }, [forgetLoaded, loadSession]);

  const handleSwitchBranch = useCallback(async (tipEntryId: string) => {
    if (!activeSession) return;
    try {
      await api.sessions.branch(activeSession, tipEntryId);
      await reloadTimeline(activeSession);
    } catch (e: any) {
      toast.error(e.message || 'Could not switch attempt');
    }
  }, [activeSession, reloadTimeline]);

  // Regenerating branches back to the user's message and runs again IN PLACE.
  // It used to fork a whole new session per attempt, which is why a chat list
  // filled up with "(regen 2)", "(regen 3)" and no way to compare them — the
  // attempts now live in one session, switchable.
  const handleRegenerate = useCallback(async (userEntryId: string, userContent: string, agentConfigId: string, sandboxId: string) => {
    if (!activeSession || !wsRef.current) return;
    try {
      await api.sessions.branch(activeSession, userEntryId);
      await reloadTimeline(activeSession);
      setActivePanel(null);
      // Empty input: the run answers the branch we just switched to rather
      // than adding a new user message. The server maps it to an empty item list.
      const payload: Record<string, any> = { session_id: activeSession, input: '', agent_config_id: agentConfigId };
      if (sandboxId) payload.sandbox_id = sandboxId;
      if (!wsRef.current.send(EV.runCreate, payload)) {
        toast.error('WebSocket disconnected — message not sent');
      }
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
    setActivePanel(null);
    if (window.innerWidth < 768) setSidebarOpen(false);
  }, []);

  if (!authed && checkError) return (
    <ThemeProvider>
      <div className="login-page">
        <div style={{ display: 'flex', flexDirection: 'column', gap: 16, alignItems: 'center' }}>
          <img src="/icon.svg" width={48} height={48} />
          <Flash variant="danger">Couldn&apos;t reach the server. Check your connection and try again.</Flash>
          <Button onClick={runCheck}>Retry</Button>
        </div>
      </div>
    </ThemeProvider>
  );
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
      diagnostics={currentSS.diagnostics}
      traceRuns={currentSS.traceRuns}
      liveRunId={currentSS.liveRunId}
      liveStartedAt={currentSS.liveStartedAt}
      liveAgentName={currentSS.liveAgentName}
      awaiting={!!activeSession && awaitingSessions.has(activeSession)}
      tasks={currentSS.tasks}
      taskView={currentSS.taskView}
      onWatchTask={watchTask}
      onUnwatchTask={unwatchTask}
      onPatchTask={patchTask}
      onSend={handleSend}
      onCancel={handleCancel}
      onApprove={handleApprove}
      onReject={handleReject}
      onFork={handleFork}
      hasMore={currentSS.hasMore}
      loadingMore={currentSS.loadingMore}
      onLoadEarlier={handleLoadEarlier}
      onSwitchBranch={handleSwitchBranch}
      onRegenerate={handleRegenerate}
      settingsReloadKey={settingsReloadKey}
      panel={activePanel}
      onPanelChange={setActivePanel}
      onTerminalOpen={handleTerminalOpen}
    />
  );

  return (
    <ThemeProvider>
      <AppShell onSettingsOpen={() => setSettingsOpen(true)} sidebarPane={sidebarPane} sidebarOpen={sidebarOpen} onSidebarToggle={setSidebarOpen}>
        {main}
        {terminalEverOpened && (
          <React.Suspense fallback={null}>
            <TerminalPanel
              open={terminalOpen}
              onClose={() => setTerminalOpen(false)}
              settingsReloadKey={settingsReloadKey}
              openRequest={terminalRequest}
            />
          </React.Suspense>
        )}
      </AppShell>
      {settingsOpen && <SettingsDialog onClose={() => { setSettingsOpen(false); setSettingsReloadKey(k => k + 1); }} />}
      <GlobalToast />
    </ThemeProvider>
  );
}
