import React, { useState, useCallback, useEffect, useRef, useMemo, memo } from 'react';
import { TextInput, Dialog, NavList as PrimerNavList, Flash, Button } from '@primer/react';
import {
  DependabotIcon, McpIcon, ShieldCheckIcon, ZapIcon, CpuIcon, PlugIcon, WorkflowIcon,
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
import { syncTaskCard } from '@/lib/streamReducer';
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

// Ordered so each section builds on the ones above it: a provider is what an
// agent talks to, an agent is what a workflow step runs, then what an agent
// attaches (tools, execution, state, the checks around it). General last.
const DIALOG_TABS: { key: string; label: string; icon: Icon; load: () => Promise<{ default: React.ComponentType }> }[] = [
  { key: 'providers',  label: 'Providers',  icon: CpuIcon,        load: () => import('@/features/providers/ProviderPanel') },
  { key: 'agents',     label: 'Agents',     icon: DependabotIcon, load: () => import('@/features/agents/AgentConfigPanel') },
  { key: 'workflows',  label: 'Workflows',  icon: WorkflowIcon,   load: () => import('@/features/workflows/WorkflowPanel') },
  { key: 'mcp',        label: 'MCP',        icon: McpIcon,        load: () => import('@/features/mcp/McpServerPanel') },
  { key: 'skills',     label: 'Skills',     icon: ZapIcon,        load: () => import('@/features/skills/SkillsPanel') },
  { key: 'sandbox',    label: 'Sandbox',    icon: ContainerIcon,  load: () => import('@/features/sandbox/SandboxPanel') },
  { key: 'memory',     label: 'Memory',     icon: DatabaseIcon,   load: () => import('@/features/memory/MemoryPanel') },
  { key: 'guardrails', label: 'Guardrails', icon: ShieldCheckIcon, load: () => import('@/features/guardrails/GuardrailPanel') },
  { key: 'plugins',    label: 'Plugins',    icon: PlugIcon,       load: () => import('@/features/plugins/PluginsPanel') },
  { key: 'general',    label: 'General',    icon: GearIcon,       load: () => import('@/features/settings/SettingsPanel') },
];

function SettingsDialog({ onClose }: { onClose: () => void }) {
  const [tab, setTab] = useState('providers');
  const [TabComp, setTabComp] = useState<React.ComponentType | null>(null);

  useEffect(() => {
    // The previous panel stays on screen while the next chunk loads — clearing
    // first blanked the dialog on every switch, even for already-cached
    // modules (the import still resolves a microtask later). The stale flag
    // drops a resolution that no longer matches the selected tab: a slow
    // first-load chunk must not overwrite a faster later click's panel.
    let stale = false;
    const entry = DIALOG_TABS.find(t => t.key === tab);
    if (!entry) return;
    entry.load().then(mod => {
      if (!stale) setTabComp(() => mod.default);
    });
    return () => { stale = true; };
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

// PLAN_COMMAND is the one slash command the composer takes: a prefix that puts
// the session into plan mode before the request it leads runs.
const PLAN_COMMAND = /^\/plan\b[ \t]*/;

function panelKey(p: InspectorPanel): string {
  if (!p) return '';
  if (p.kind === 'task') return `task/${p.taskId}`;
  return p.kind;
}

function readHash(): { sessionId: string | null; panel: InspectorPanel } {
  const h = window.location.hash;
  const m = /^#\/session\/([a-zA-Z0-9_-]+)(?:\/(trace|tasks|context|task\/([a-zA-Z0-9_-]+)))?$/.exec(h);
  if (!m) return { sessionId: null, panel: null };
  let panel: InspectorPanel = null;
  if (m[2] === 'trace') panel = { kind: 'trace' };
  else if (m[2] === 'tasks') panel = { kind: 'tasks' };
  else if (m[2] === 'context') panel = { kind: 'context' };
  else if (m[3]) panel = { kind: 'task', taskId: m[3] };
  return { sessionId: m[1], panel };
}

function writeHash(sessionId: string | null, panel: InspectorPanel) {
  let next = '';
  if (sessionId) {
    next = `#/session/${sessionId}`;
    if (panel?.kind === 'trace') next += '/trace';
    else if (panel?.kind === 'tasks') next += '/tasks';
    else if (panel?.kind === 'context') next += '/context';
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
  // Bumped by workflow.updated: the chat's workflow strip and Tasks panel
  // refetch when a background sequence moves.
  const [workflowTick, setWorkflowTick] = useState(0);
  const [settingsReloadKey, setSettingsReloadKey] = useState(0);
  // The active session's display name and sandbox binding. Captured from the
  // existence-check fetch below and kept fresh by the title_updated /
  // sandbox_bound events; the id guards against a stale response landing after
  // a session switch.
  const [sessionMeta, setSessionMeta] = useState<{ id: string; name: string; sandboxId: string; workDir: string } | null>(null);
  // What the composer's plan checkbox shows: seeded from the session, then the
  // person's own intent until the next message carries it. planDirty marks a
  // hand toggle not yet delivered — only then does a send carry a `plan` flag;
  // an untouched box sends nothing, so a re-sent stale value can't re-arm a
  // session the server just unlocked (plan approved between run end and the
  // refetch).
  const [planning, setPlanning] = useState(false);
  const [planDirty, setPlanDirty] = useState(false);
  const handlePlanningChange = useCallback((v: boolean) => {
    setPlanning(v);
    setPlanDirty(true);
  }, []);
  // Bindings announced over the socket, per session. The session GET races the
  // session.sandbox_bound broadcast (meta is cleared before the fetch, so the
  // event can arrive while prev is null), and a binding is immutable once set
  // — any announced value is THE value, merged over whatever the slower GET
  // returns.
  const announcedBindings = useRef<Record<string, { sandboxId: string; workDir: string }>>({});
  // Bumped whenever the set of bound sessions changes (a new binding lands, a
  // session is deleted): the recent-projects pickers in ChatView and the
  // terminal panel aggregate over the sessions list, and without this they
  // would only ever show the world as of their mount.
  const [bindingsVersion, setBindingsVersion] = useState(0);
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
  const [terminalRequest, setTerminalRequest] = useState<{ id: string; name: string; workDir?: string; nonce: number } | null>(null);
  const terminalOpenRef = useRef(false);
  terminalOpenRef.current = terminalOpen;
  const terminalNonceRef = useRef(0);
  const handleTerminalOpen = useCallback((sandbox?: { id: string; name: string; workDir?: string }) => {
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

  const { wsRef, sessionRunRef, loadSession, loadTraces, deleteSession, loadEarlier, forgetLoaded, watchTask, unwatchTask } = useAgentSocket(updateSS);

  // patchTask applies a server-confirmed task state change (e.g. the stop
  // API's response) directly — the fallback for when no hub broadcast will
  // come (stopping a paused task after a restart).
  const patchTask = useCallback((sid: string, taskId: string, patch: Record<string, unknown>) => {
    updateSS(sid, s => {
      const cur = s.tasks[taskId];
      if (!cur) return s;
      // Stamped like the live-event path (updateTask), because the duration a
      // terminal task shows is updatedAt - createdAt: without this it freezes
      // at whatever event last touched the task — a tool call, an approval —
      // and a task stopped after a long quiet stretch shows its timer jumping
      // backwards. A patch carrying its own value still wins.
      const next = { ...s, tasks: { ...s.tasks, [taskId]: { ...cur, updatedAt: Date.now(), ...patch } } };
      // The spawn card follows the same confirmation. A retry normally re-arms
      // it from the run.started broadcast, but the caller that got this answer
      // over REST may have no socket at all — and then nothing else would ever
      // correct a card still offering to retry a task that is already running.
      const merged = next.tasks[taskId];
      if (!cur.toolCallId) return next;
      const msgs = syncTaskCard(next.messages, cur.toolCallId, {
        id: taskId, label: merged.label, status: merged.status,
        summary: merged.summary, attempt: merged.attempt,
      });
      return msgs ? { ...next, messages: msgs } : next;
    });
  }, [updateSS]);

  useEffect(() => {
    setSessionMeta(null);
    // The checkbox belongs to the session: disarm on every switch (and on New
    // Chat) so the previous session's armed state can't leak into this one
    // during the window before the GET below seeds the real phase.
    setPlanning(false);
    setPlanDirty(false);
    if (!activeSession) return;
    let cancelled = false;
    // The session id can come from the URL hash and may not exist (stale link,
    // deleted session, hand-typed id). The messages endpoint returns [] for an
    // unknown session rather than 404, so validate existence explicitly: a 404
    // means drop the id — the app falls back to the empty state and typing then
    // starts a new session instead of running against a non-existent session.
    const tryLoad = () => loadSession(activeSession).catch(() => toast.error('Could not load conversation'));
    api.sessions.get(activeSession)
      .then((sess) => {
        if (cancelled) return;
        const s = sess as { name?: string; sandbox_id?: string; work_dir?: string; planning?: boolean };
        // A binding announced while this fetch was in flight wins: the fetch
        // read the row before the bind landed, and bindings never change.
        const announced = announcedBindings.current[activeSession];
        setSessionMeta({
          id: activeSession,
          name: s?.name || '',
          sandboxId: announced ? announced.sandboxId : (s?.sandbox_id || ''),
          workDir: announced ? announced.workDir : (s?.work_dir || ''),
        });
        setPlanning(!!s?.planning);
        tryLoad();
      })
      .catch((e: { status?: number }) => {
        if (cancelled) return;
        if (e?.status === 404) setActiveSession(null);
        else tryLoad(); // transient error — try loading anyway
      });
    return () => { cancelled = true; };
  }, [activeSession, loadSession]);

  // An approved plan unlocks the SESSION mid-run, so the checkbox is re-read
  // when a run ENDS — the phase may have changed with nobody touching the box.
  // Only on the true→false edge: session open already seeded it above.
  const activeRunning = activeSession ? !!ss[activeSession]?.running : false;
  const wasRunning = useRef(false);
  useEffect(() => {
    const ended = wasRunning.current && !activeRunning;
    wasRunning.current = activeRunning;
    if (!ended || !activeSession) return;
    let cancelled = false;
    api.sessions.get(activeSession)
      .then(sess => { if (!cancelled) setPlanning(!!(sess as { planning?: boolean }).planning); })
      .catch(() => {});
    return () => { cancelled = true; };
  }, [activeSession, activeRunning, ss]);

  // Persisted traces backfill on the first open of a lens that reads them —
  // the trace panel, or the context panel (it joins its items to spans). Also
  // covers deep links (#/session/x/trace): the hash seeds activePanel.
  useEffect(() => {
    if (!activeSession) return;
    if (activePanel?.kind === 'trace' || activePanel?.kind === 'context') loadTraces(activeSession);
  }, [activeSession, activePanel, loadTraces]);

  useEffect(() => {
    if (!wsRef.current) return;
    // Single handler per event type — WSClient.on replaces per type, so each
    // event's full behavior lives in one body (a second .on would clobber it).
    wsRef.current.on(EV.sessionTitleUpdated, (p: { session_id?: string; title?: string }) => {
      setSessionReloadKey(k => k + 1);
      if (p?.session_id && typeof p.title === 'string') {
        const title = p.title;
        setSessionMeta(prev => (prev && prev.id === p.session_id ? { ...prev, name: title } : prev));
      }
    });
    // A workflow execution moved in some session. The strip and Tasks panel
    // read REST, and a hidden step has no parent run to flip `running`, so this
    // is the only signal to refetch. A single counter is enough — the strip is
    // scoped to the open session, so at worst one cheap refetch of it.
    wsRef.current.on(EV.workflowUpdated, () => setWorkflowTick(k => k + 1));
    wsRef.current.on(EV.sessionSandboxBound, (p: { session_id?: string; sandbox_id?: string; work_dir?: string }) => {
      if (p?.session_id && p.sandbox_id) {
        // Record first, then patch the live meta. The record is what makes the
        // announcement survive the meta being null (session switch mid-fetch):
        // the fetch merges it when it lands.
        announcedBindings.current[p.session_id] = { sandboxId: p.sandbox_id, workDir: p.work_dir || '' };
        setSessionMeta(prev => (prev && prev.id === p.session_id
          ? { ...prev, sandboxId: p.sandbox_id!, workDir: p.work_dir || '' }
          : prev));
        // A new (sandbox, workdir) pair exists: refresh the project pickers.
        setBindingsVersion(v => v + 1);
      }
    });
  }, [wsRef]);

  const handleSend = useCallback(async (input: string, agentConfigId?: string, sandboxId?: string, workDir?: string) => {
    if (!wsRef.current) return;
    if (!wsRef.current.isConnected()) {
      toast.error('WebSocket disconnected — message not sent');
      return;
    }
    // `/plan` asks for a plan before any change — the keyboard half of the
    // composer's toggle. It is handled HERE, not in the composer, because it
    // sets the SESSION's phase and a brand-new session has no id until the
    // block below creates one.
    const planned = PLAN_COMMAND.test(input);
    const text = planned ? input.replace(PLAN_COMMAND, '') : input;
    // The command is a hand toggle too: dirty makes a bare "/plan" carry the
    // phase on the NEXT message.
    if (planned) { setPlanning(true); setPlanDirty(true); }
    if (planned && !text.trim()) return; // arms the checkbox for the next message
    // Typing straight into the box with no active session starts a new session,
    // instead of silently dropping the message. The freshly-created session has
    // no history, so mark it loaded to protect the optimistic message from the
    // load-session effect.
    let sid = activeSession;
    let isNew = false;
    if (!sid) {
      try {
        const sess = await api.sessions.create('New Session', agentConfigId) as { id: string };
        sid = sess.id;
        isNew = true;
        setActiveSession(sid);
        setActivePanel(null);
        setSessionReloadKey(k => k + 1);
      } catch {
        toast.error('Could not start a new session');
        return;
      }
    }
    const clientMsgId = nextClientMsgId();
    updateSS(sid, s => ({ ...s, messages: [...s.messages, { role: 'user', content: text, clientMsgId }], ...(isNew ? { loaded: true } : {}) }));
    // The phase travels WITH the message — but only a message that carries a
    // hand toggle says anything: an absent `plan` leaves the session's phase
    // alone, so a send can't re-arm a session the server unlocked mid-window.
    const payload: Record<string, any> = { session_id: sid, input: text, agent_config_id: agentConfigId };
    if (planned || planDirty) payload.plan = planned || planning;
    if (sandboxId) {
      payload.sandbox_id = sandboxId;
      if (workDir) payload.work_dir = workDir;
    }
    if (!wsRef.current.send(EV.runCreate, payload)) {
      // The socket dropped between the isConnected() check and the send: roll
      // back the optimistic bubble so it isn't left stranded with no run.
      updateSS(sid, s => ({ ...s, messages: s.messages.filter((m: { clientMsgId?: string }) => m.clientMsgId !== clientMsgId) }));
      toast.error('WebSocket disconnected — message not sent');
      return;
    }
    // Delivered: the toggle reached the server, the box is clean again.
    setPlanDirty(false);
  }, [activeSession, planning, planDirty, updateSS, wsRef]);

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
    // The record of the session's announced binding dies with it — the map
    // would otherwise grow one entry per bound session for the page's life.
    delete announcedBindings.current[deletedId];
    // The deleted session may have carried the last reference to a project —
    // the pickers re-aggregate.
    setBindingsVersion(v => v + 1);
  }, [deleteSession]);

  const handleLoadEarlier = useCallback(() => {
    if (!activeSession) return;
    const s = ss[activeSession];
    if (!s?.hasMore || s.loadingMore || s.entries.length === 0) return;
    const oldest = s.entries[0]?.id;
    if (oldest) loadEarlier(activeSession, oldest);
  }, [activeSession, ss, loadEarlier]);

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

  // The Context panel's "Compact now": one forced pass, then the timeline
  // reload — the fold marks entries compacted and appends a checkpoint, which
  // no local patch can express. Toasts carry the outcome either way; errors
  // (409 while a run is live, 400 when compaction is off) surface their
  // message.
  const handleCompact = useCallback(async () => {
    if (!activeSession) return;
    try {
      const res = await api.sessions.compact(activeSession) as { compacted?: boolean; before_items?: number; after_items?: number };
      if (res.compacted) {
        toast.success(`Compacted ${res.before_items} items into ${res.after_items}`);
        await reloadTimeline(activeSession);
      } else {
        toast.info('Nothing to fold — the kept window already covers the history');
      }
    } catch (e: any) {
      toast.error(e.message || 'Compaction failed');
    }
  }, [activeSession, reloadTimeline]);

  // Regenerating branches back to the user's message and runs again IN PLACE.
  // It used to fork a whole new session per attempt, which is why a chat list
  // filled up with "(regen 2)", "(regen 3)" and no way to compare them — the
  // attempts now live in one session, switchable.
  const handleRegenerate = useCallback(async (userEntryId: string, userContent: string, agentConfigId: string, sandboxId: string, workDir?: string) => {
    if (!activeSession || !wsRef.current) return;
    try {
      await api.sessions.branch(activeSession, userEntryId);
      await reloadTimeline(activeSession);
      // The Inspector stays open: regen is in-place (same session), so an open
      // trace/task panel remains valid — the replaced attempt gets its "replaced"
      // chip and the drawer follows the new live run. (The close that used to sit
      // here was a leftover from the fork-a-new-session implementation.)
      // Empty input: the run answers the branch we just switched to rather
      // than adding a new user message. The server maps it to an empty item list.
      const payload: Record<string, any> = { session_id: activeSession, input: '', agent_config_id: agentConfigId };
      if (sandboxId) {
        payload.sandbox_id = sandboxId;
        // A regen can be an unbound session's first sandbox-carrying run, so
        // the workdir choice rides along; a bound session ignores it anyway.
        if (workDir) payload.work_dir = workDir;
      }
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

  // Stable reference so MemoizedChatView's shallow compare isn't defeated by a
  // fresh object literal every render.
  const sessionBinding = useMemo(() =>
    sessionMeta && sessionMeta.id === activeSession && sessionMeta.sandboxId
      ? { sandboxId: sessionMeta.sandboxId, workDir: sessionMeta.workDir }
      : null,
  [sessionMeta, activeSession]);

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
      sessionName={sessionMeta && sessionMeta.id === activeSession ? sessionMeta.name : ''}
      sessionBinding={sessionBinding}
      messages={currentSS.messages}
      entries={currentSS.entries}
      loaded={currentSS.loaded}
      streaming={currentSS.streaming}
      reasoning={currentSS.reasoning}
      running={currentSS.running}
      workflowTick={workflowTick}
      planning={planning}
      onPlanningChange={handlePlanningChange}
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
      onCompact={handleCompact}
      onRegenerate={handleRegenerate}
      settingsReloadKey={settingsReloadKey}
      bindingsVersion={bindingsVersion}
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
              bindingsVersion={bindingsVersion}
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
