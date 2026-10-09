import type { AttachmentMeta } from '@/lib/attachments';
import React, { useState, useCallback, useEffect, useRef, useMemo, memo } from 'react';
import { Flash, Button, IconButton } from '@primer/react';
import {
  DependabotIcon, McpIcon, ShieldCheckIcon, SparkleIcon, CpuIcon,
  ContainerIcon, DatabaseIcon, FileDirectoryIcon, GearIcon, PersonIcon, PeopleIcon, CommentDiscussionIcon, LogIcon, PlusIcon, WorkflowIcon,
} from '@primer/octicons-react';
import { ThemeProvider } from '@/theme/ThemeProvider';
import { AppShell } from '@/layout/AppShell';
import { GlobalToast } from '@/layout/GlobalToast';
import { LoginPage, exchangeErrorTag } from '@/layout/LoginPage';
import { PanelDialog, type DialogTab } from '@/layout/PanelDialog';
import { SessionList as SessionListImpl } from '@/features/sessions/SessionList';
import { AttentionSignals } from '@/features/sessions/AttentionSignals';
import { ChatView, type ChatViewActions, type InspectorPanel } from '@/features/chat/ChatView';
import { ErrorBoundary } from '@/components/ErrorBoundary';

// Lazy: the xterm chunk loads when the terminal panel first opens; the panel
// then stays mounted while hidden — invariant 81.
const TerminalPanel = React.lazy(() =>
  import('@/features/terminal/TerminalPanel').then(m => ({ default: m.TerminalPanel })),
);
import { checkAuth, getToken, api, exchangeCode, TOKEN_KEY } from '@/lib/api';
import { EV, TASK_KIND_WORKFLOW, type InjectQueue, type SessionStatus, type SessionStatusEvent } from '@/lib/protocol';
import { WorkflowsHub, type HubTab } from '@/features/workflows/WorkflowsHub';
import { WORKFLOW_COMMAND } from '@/features/chat/SlashMenu';
import { SESSION_REMOVED } from '@/features/sessions/SessionPicker';
import { useAgentSocket, defaultSS, type SessionState } from '@/lib/useAgentSocket';
import { patchToolCall, type ToolCallPatch } from '@/lib/timeline';
import { syncTaskCard } from '@/lib/streamReducer';
import { adoptNewSessionPrefs, clearSessionPrefs } from '@/lib/drafts';
import { toast } from '@/lib/toast';
import { putBackInComposer } from '@/lib/composer';
import { MeContext, useMeLoader } from '@/lib/me';
import { useAuthMode } from '@/lib/authMode';
import { useNarrow } from '@/lib/hooks';
import { readHash, writeHash, consumeAuthFragment, restoreReturnHash } from '@/lib/route';
import { frameTooLarge } from '@/lib/messageSize';
import { installExternalLinkOpener } from '@/lib/externalLinks';

// The settings hub's tabs: the person's own, then what a run is built from,
// the admin entries last — invariant 61. Workflows are authored in the sidebar's hub.
const scopedTab = (name: 'ProvidersTab' | 'AgentsTab' | 'McpServersTab' | 'SkillsTab') =>
  () => import('@/features/settings/ScopedEntityPanel').then(m => ({ default: m[name] }));

const SETTINGS_TABS: DialogTab[] = [
  { key: 'account',    label: 'Account',     icon: PersonIcon,      load: () => import('@/features/account/AccountPanel') },
  { key: 'general',    label: 'General',     icon: GearIcon,        load: () => import('@/features/settings/SettingsPanel') },
  { key: 'providers',  label: 'Providers',   icon: CpuIcon,         load: scopedTab('ProvidersTab'), scoped: true, dividerBefore: true },
  { key: 'agents',     label: 'Agents',      icon: DependabotIcon,  load: scopedTab('AgentsTab'), scoped: true },
  { key: 'mcp',        label: 'MCP servers', icon: McpIcon,         load: scopedTab('McpServersTab'), scoped: true },
  { key: 'skills',     label: 'Skills',      icon: SparkleIcon,     load: scopedTab('SkillsTab'), scoped: true },
  { key: 'sandbox',    label: 'Sandboxes',   icon: ContainerIcon,   load: () => import('@/features/sandbox/SandboxPanel') },
  { key: 'memory',     label: 'Memory',      icon: DatabaseIcon,    load: () => import('@/features/memory/MemoryPanel') },
  { key: 'guardrails', label: 'Guardrails',  icon: ShieldCheckIcon, load: () => import('@/features/guardrails/GuardrailPanel') },
];

// Administration: people, then what members own, then the record of it all.
const ADMIN_TABS: DialogTab[] = [
  { key: 'members',   label: 'Members',    icon: PeopleIcon,            load: () => import('@/features/admin/MembersPanel') },
  { key: 'sessions',  label: 'Sessions',   icon: CommentDiscussionIcon, load: () => import('@/features/admin/SessionsPanel') },
  { key: 'projects',  label: 'Projects',   icon: FileDirectoryIcon,     load: () => import('@/features/admin/ProjectsPanel') },
  { key: 'workflows', label: 'Workflows',  icon: WorkflowIcon,          load: () => import('@/features/admin/ScopedRowsPanel').then(m => ({ default: m.AdminWorkflows })) },
  { key: 'audit',     label: 'Audit logs', icon: LogIcon,               load: () => import('@/features/admin/AuditPanel') },
];

const DEFAULT_SS = defaultSS();

// A session id's width, for sizing a message's frame before its session exists.
const PLACEHOLDER_SESSION_ID = '00000000-0000-0000-0000-000000000000';

// Monotonic id stamped on each optimistic user bubble: the socket layer rolls
// back one specific un-sent message by it, and the reducer dedups identical sends.
let clientMsgSeq = 0;
function nextClientMsgId(): string { return 'c' + (++clientMsgSeq); }

const MemoizedChatView = memo(ChatView);
// A streaming frame moves no sidebar prop, so the list does not redraw per frame.
const MemoizedSessionList = memo(SessionListImpl);

// The composer's plan-mode prefix (invariant 33); WORKFLOW_COMMAND lives with
// the menu that types it.
const PLAN_COMMAND = /^\/plan\b[ \t]*/;
// "/plan off <message>" leaves plan mode.
const PLAN_OFF_COMMAND = /^\/plan[ \t]+off\b[ \t]*/;

function panelKey(p: InspectorPanel): string {
  if (!p) return '';
  if (p.kind === 'task') return `task/${p.taskId}`;
  return p.kind;
}

// Login-callback state, captured (and stripped from the URL) once at module
// load — before any render, so StrictMode's double init cannot consume it twice.
const AUTH_FRAGMENT = consumeAuthFragment();

function App() {
  const [authError, setAuthError] = useState(AUTH_FRAGMENT.error || '');
  const [authed, setAuthed] = useState(!!getToken());
  // The signed-in user, fetched once authenticated and shared by context; the
  // role shapes what the settings dialog offers. isAdmin is null until known.
  const meState = useMeLoader(authed);
  const me = meState.me;
  const isAdmin = meState.loading ? null : me?.role === 'admin';
  // Token mode is one person: the Members tab has nobody to list.
  const authMode = useAuthMode();
  const [checking, setChecking] = useState(true);
  // The initial auth check failed at the network level (not "not authenticated"):
  // shown as a retryable error, never a blank screen.
  const [checkError, setCheckError] = useState('');
  const [activeSession, setActiveSession] = useState<string | null>(() => readHash().sessionId);
  const activeSessionRef = useRef(activeSession);
  activeSessionRef.current = activeSession;
  const [activePanel, setActivePanel] = useState<InspectorPanel>(() => readHash().panel);
  // The Workflows hub, when it is the open view (null = a conversation).
  const [hubTab, setHubTab] = useState<HubTab | null>(() => readHash().hub);
  const [settingsOpen, setSettingsOpen] = useState(() => readHash().settings != null);
  const [settingsTab, setSettingsTab] = useState<string | undefined>(() => readHash().settings || undefined);
  const [sidebarOpen, setSidebarOpen] = useState(false);
  const [sessionReloadKey, setSessionReloadKey] = useState(0);
  // Bumped when the settings dialog closes: the composer's pickers and the
  // terminal panel refetch the configuration it may have changed.
  const [settingsReloadKey, setSettingsReloadKey] = useState(0);
  const narrow = useNarrow();
  const narrowRef = useRef(narrow);
  narrowRef.current = narrow;
  // The active session's name and binding, from the existence-check fetch below
  // and the title_updated / project_bound events; the id rejects a response that
  // lands after a session switch.
  const [sessionMeta, setSessionMeta] = useState<{ id: string; name: string; projectId: string; agentConfigId: string } | null>(null);
  // Bindings announced over the socket, per session: the broadcast can land while
  // the session GET is in flight (meta is null then), and an announced binding
  // wins over what the GET returns — invariant 27.
  const announcedBindings = useRef<Record<string, string>>({});
  // Bumped when the set of bound sessions changes: a bind can auto-create its
  // scratch project, so the project pickers (ChatView, terminal panel) refetch.
  const [bindingsVersion, setBindingsVersion] = useState(0);
  // Global terminal panel, opened from the composer (closing lives on the panel).
  // everOpened defers the mount to first use; then it stays mounted while hidden.
  const [terminalOpen, setTerminalOpen] = useState(false);
  const [terminalEverOpened, setTerminalEverOpened] = useState(false);
  // A one-shot "start a terminal for this project" request, set only when the
  // composer opens a CLOSED panel with a project selected; the nonce tells
  // repeats apart.
  const [terminalRequest, setTerminalRequest] = useState<{ projectId: string; projectName?: string; targetName?: string; nonce: number } | null>(null);
  const terminalOpenRef = useRef(false);
  terminalOpenRef.current = terminalOpen;
  const terminalNonceRef = useRef(0);
  const handleTerminalOpen = useCallback((project?: { projectId: string; projectName?: string; targetName?: string }) => {
    if (!terminalOpenRef.current && project) {
      setTerminalRequest({ ...project, nonce: ++terminalNonceRef.current });
    }
    setTerminalOpen(true);
    setTerminalEverOpened(true);
  }, []);

  const [ss, setSS] = useState<Record<string, SessionState>>({});
  // The latest state for callbacks that read it without depending on it —
  // a callback rebuilt per streaming frame would re-render the memoized view.
  const ssRef = useRef(ss);
  ssRef.current = ss;

  const runCheck = useCallback(() => {
    setChecking(true);
    setCheckError('');
    checkAuth()
      .then(ok => { setAuthed(ok); setChecking(false); })
      // A network failure or a non-refusal status (429, 502) rejects here:
      // leave "checking", stay signed in, show the retry screen.
      .catch(e => {
        setChecking(false);
        const status = (e as { status?: number } | null)?.status;
        setCheckError(status === 429
          ? 'Too many requests from your address — wait a minute and retry.'
          : status ? `The server answered HTTP ${status} — try again.`
          : 'Couldn\'t reach the server. Check your connection and try again.');
      });
  }, []);

  useEffect(() => {
    // An OAuth callback landed us here: trade the one-time code for the
    // session token instead of probing a credential that doesn't exist yet.
    if (AUTH_FRAGMENT.code) {
      exchangeCode(AUTH_FRAGMENT.code)
        .then(() => { setAuthed(true); setChecking(false); restoreReturnHash(); })
        .catch(e => {
          setAuthError(exchangeErrorTag(e));
          setChecking(false);
        });
      return;
    }
    runCheck();
  }, [runCheck]);

  // The URL names the view, Settings rides as `?settings=<tab>` (invariant 61).
  // A view move or Settings opening pushes a history entry; a lens or a tab
  // replaces it.
  const writtenViewRef = useRef<string | null>(null);
  const settingsWasOpenRef = useRef(settingsOpen);
  // Whether the entry on screen is the one Settings pushed, so closing it
  // goes Back instead of leaving a dead entry; set when the close is that.
  const settingsPushedRef = useRef(false);
  const closingBackRef = useRef(false);
  useEffect(() => {
    const view = hubTab ? 'hub:' + hubTab : 'session:' + (activeSession || '');
    const viewMoved = writtenViewRef.current !== null && writtenViewRef.current !== view;
    writtenViewRef.current = view;
    const opening = settingsOpen && !settingsWasOpenRef.current;
    settingsWasOpenRef.current = settingsOpen;
    if (closingBackRef.current) {
      // The close went Back: the popped entry is the view already.
      closingBackRef.current = false;
      return;
    }
    if (viewMoved) settingsPushedRef.current = false;
    if (opening) settingsPushedRef.current = true;
    writeHash(activeSession, activePanel, hubTab, settingsOpen ? (settingsTab ?? '') : null, viewMoved || opening);
  }, [activeSession, activePanel, hubTab, settingsOpen, settingsTab]);

  const closeSettings = useCallback(() => {
    setSettingsOpen(false);
    setSettingsTab(undefined);
    setSettingsReloadKey(k => k + 1);
    setSessionReloadKey(k => k + 1);
    if (settingsPushedRef.current && readHash().settings != null) {
      settingsPushedRef.current = false;
      closingBackRef.current = true;
      window.history.back();
    }
  }, []);

  // A lens belongs to a conversation: none open, none shown.
  useEffect(() => {
    if (!activeSession) setActivePanel(null);
  }, [activeSession]);

  // The URL is read back on every navigation (hashchange and popstate both fire;
  // the handler is idempotent); the settings parameter opens or closes the dialog.
  useEffect(() => {
    const onHash = () => {
      const { sessionId, panel, hub, settings } = readHash();
      if (settings != null) {
        setSettingsTab(settings || undefined);
        setSettingsOpen(true);
      } else {
        setSettingsOpen(open => {
          if (open) { setSettingsTab(undefined); setSettingsReloadKey(k => k + 1); setSessionReloadKey(k => k + 1); }
          return false;
        });
        settingsPushedRef.current = false;
      }
      setHubTab(hub);
      if (hub) return; // the conversation beside the hub stays as it was
      setActiveSession(prev => prev === sessionId ? prev : sessionId);
      setActivePanel(prev => panelKey(prev) === panelKey(panel) ? prev : panel);
    };
    window.addEventListener('hashchange', onHash);
    window.addEventListener('popstate', onHash);
    return () => {
      window.removeEventListener('hashchange', onHash);
      window.removeEventListener('popstate', onHash);
    };
  }, []);

  // A link in rendered markdown opens elsewhere; the page stays.
  useEffect(() => installExternalLinkOpener(), []);

  useEffect(() => {
    // A logout is a definitive "not authenticated" — clear any lingering
    // network-error state so the login page shows, not the retry screen.
    const handler = () => { setAuthed(false); setCheckError(''); };
    window.addEventListener('auth:logout', handler);
    return () => window.removeEventListener('auth:logout', handler);
  }, []);

  // Another tab signed out (the persisted token went) or in (one appeared
  // while this tab shows the login page): a fresh document follows suit.
  useEffect(() => {
    const handler = (e: StorageEvent) => {
      if (e.storageArea !== localStorage || e.key !== TOKEN_KEY) return;
      if ((authed && !e.newValue) || (!authed && e.newValue)) window.location.reload();
    };
    window.addEventListener('storage', handler);
    return () => window.removeEventListener('storage', handler);
  }, [authed]);

  const updateSS = useCallback((sid: string, fn: (s: SessionState) => SessionState) => {
    setSS(prev => {
      const cur = prev[sid] || defaultSS();
      const next = fn(cur);
      return next === cur ? prev : { ...prev, [sid]: next };
    });
  }, []);

  // Whether the open session's OWN run waits on a decision (a task's pause is
  // not that): read off the session detail's pending calls, re-read on each
  // status announcement.
  const [ownPending, setOwnPending] = useState<{ id: string; pending: boolean } | null>(null);
  const ownPendingGen = useRef(0);
  const readOwnPending = useCallback((sid: string, detail: unknown) => {
    const calls = (detail as { pending?: Array<{ task_id?: string }> } | null)?.pending || [];
    setOwnPending({ id: sid, pending: calls.some(c => !c.task_id) });
  }, []);
  const refreshOwnPending = useCallback((sid: string) => {
    const gen = ++ownPendingGen.current;
    api.sessions.get(sid)
      .then(detail => { if (gen === ownPendingGen.current && activeSessionRef.current === sid) readOwnPending(sid, detail); })
      .catch(() => undefined);
  }, [readOwnPending]);

  // The sidebar's markers: each session's status as last announced
  // (session.status), over the one its list row carries — invariant 86.
  const [sessionStatuses, setSessionStatuses] = useState<Record<string, SessionStatus>>({});

  // What the socket says about a session beyond its runs: the auto-title and
  // the project binding (recorded in announcedBindings, which outlives a cleared meta).
  const sessionEvents = useMemo(() => ({
    activeSession: () => activeSessionRef.current,
    onTitleUpdated: (sid: string, title: string) => {
      setSessionMeta(prev => (prev && prev.id === sid ? { ...prev, name: title } : prev));
    },
    onProjectBound: (sid: string, projectId: string) => {
      announcedBindings.current[sid] = projectId;
      setSessionMeta(prev => (prev && prev.id === sid ? { ...prev, projectId } : prev));
      setBindingsVersion(v => v + 1);
    },
    onStatus: (st: SessionStatusEvent | null) => {
      setSessionStatuses(prev => {
        if (!st) return Object.keys(prev).length === 0 ? prev : {};
        return prev[st.session_id] === st.status ? prev : { ...prev, [st.session_id]: st.status };
      });
      const sid = st ? st.session_id : activeSessionRef.current;
      if (!sid || sid !== activeSessionRef.current) return;
      // Nothing waited on needs no read; otherwise the detail says whose call it is.
      if (st && st.pending_count === 0) {
        ownPendingGen.current++;
        setOwnPending({ id: sid, pending: false });
      } else {
        refreshOwnPending(sid);
      }
    },
  }), [refreshOwnPending]);

  const { wsRef, sessionRunRef, connected, loadSession, loadTasks, loadTraces, loadSpanPayload, deleteSession, forgetLoaded, watchTask, unwatchTask, queueInput, dropQueued } = useAgentSocket(updateSS, sessionEvents);

  // patchTask applies a server-confirmed task state (the stop API's response)
  // directly, for when no hub broadcast will come (a paused task stopped after
  // a restart).
  const patchTask = useCallback((sid: string, taskId: string, patch: Record<string, unknown>) => {
    updateSS(sid, s => {
      const cur = s.tasks[taskId];
      if (!cur) return s;
      // updatedAt is stamped as the live path (updateTask) does: a terminal task's
      // duration is updatedAt - createdAt. A patch carrying its own value wins.
      const next = { ...s, tasks: { ...s.tasks, [taskId]: { ...cur, updatedAt: Date.now(), ...patch } } };
      // The spawn card follows too: the REST caller may have no socket, so the
      // run.started that would re-arm it never comes.
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
    if (!activeSession) return;
    let cancelled = false;
    // The id may come from the URL and not exist; the messages endpoint answers
    // [] for an unknown session, so existence is checked here and a 404 drops the
    // id. A failed re-read of a session on screen can only be said here.
    const tryLoad = () => loadSession(activeSession).catch(() => {
      if (ssRef.current[activeSession]?.messages.length) toast.error('Could not refresh the session');
    });
    const pendingGen = ++ownPendingGen.current;
    api.sessions.get(activeSession)
      .then((sess) => {
        if (cancelled) return;
        const s = sess as { name?: string; project_id?: string; agent_config_id?: string };
        // A re-read a status announcement started since is the newer answer.
        if (pendingGen === ownPendingGen.current) readOwnPending(activeSession, sess);
        // A binding announced while this fetch was in flight wins — invariant 27.
        const announced = announcedBindings.current[activeSession];
        setSessionMeta({
          id: activeSession,
          name: s?.name || '',
          projectId: announced || s?.project_id || '',
          // The session's server-side agent: the composer's fallback when this
          // browser holds no draft (a fork, another device).
          agentConfigId: s?.agent_config_id || '',
        });
        tryLoad();
      })
      .catch((e: { status?: number }) => {
        if (cancelled) return;
        if (e?.status === 404) setActiveSession(null);
        else tryLoad(); // transient error — try loading anyway
      });
    return () => { cancelled = true; };
  }, [activeSession, loadSession, readOwnPending]);

  // The session's trace summary, once (loadTraces guards): turn labels and the
  // trace/context lenses join to its spans; payloads stay lazy — invariant 22.
  useEffect(() => {
    if (activeSession) loadTraces(activeSession);
  }, [activeSession, loadTraces]);

  // The view's Retry after a failed first load.
  const handleRetryLoad = useCallback(() => {
    if (activeSession) loadSession(activeSession).catch(() => undefined);
  }, [activeSession, loadSession]);
  // The Tasks lens's Retry after its list failed to load.
  const handleRetryTasks = useCallback(() => {
    if (activeSession) loadTasks(activeSession);
  }, [activeSession, loadTasks]);

  // reloadTimeline re-reads a session's history after a server-side change no
  // local patch expresses: a branch move, a compaction, a note the server wrote.
  const reloadTimeline = useCallback(async (sid: string) => {
    forgetLoaded(sid);
    await loadSession(sid).catch(() => toast.error('Could not reload the session'));
  }, [forgetLoaded, loadSession]);

  // The /workflow command: the name, then the brief. With no session open it
  // makes one, as a message would (invariant 69); the reload brings in the
  // started note the server writes.
  const runWorkflowCommand = useCallback(async (rest: string, agentConfigId?: string, projectId?: string) => {
    const spec = rest.trim();
    if (!spec) {
      toast.error('Which workflow? /workflow <name> <brief>');
      return;
    }
    let workflows: { id: string; name: string }[];
    try {
      workflows = await api.workflows.list() as { id: string; name: string }[];
    } catch (e) {
      toast.error((e as Error).message || 'Could not list workflows');
      return;
    }
    // The name may hold spaces, so it is matched against the list — the
    // longest name the text starts with — and what follows it is the brief.
    const lower = spec.toLowerCase();
    const wf = workflows
      .filter(w => lower === w.name.toLowerCase() || lower.startsWith(w.name.toLowerCase() + ' '))
      .sort((a, b) => b.name.length - a.name.length)[0];
    if (!wf) {
      toast.error(workflows.length ? `No workflow named "${spec.split(/\s+/)[0]}". Available: ${workflows.map(w => w.name).join(', ')}` : 'No workflows yet — the Workflows hub in the sidebar is where to add one');
      return;
    }
    const brief = spec.slice(wf.name.length).trim();
    let sid = activeSession;
    if (!sid) {
      try {
        const sess = await api.sessions.create(agentConfigId ? { agent_config_id: agentConfigId } : {}) as { id: string };
        sid = sess.id;
        adoptNewSessionPrefs(sid);
        setActiveSession(sid);
        setActivePanel(null);
        setSessionReloadKey(k => k + 1);
      } catch {
        toast.error('Could not start a new session');
        return;
      }
    }
    try {
      // The composer's project rides along as on a message: it binds an unbound
      // session before the start — invariant 27.
      const body: { session_id: string; input: string; project_id?: string } = { session_id: sid, input: brief };
      if (projectId) body.project_id = projectId;
      await api.workflows.run(wf.id, body);
      toast.success(`Started "${wf.name}" in the background — the result comes back here`);
      // Not visible from here: with no project, bound or picked, the workflow
      // has no file or command tools.
      const bound = (sessionMeta && sessionMeta.id === sid ? !!sessionMeta.projectId : false) || !!projectId;
      if (!bound) toast.info('This session has no project — the workflow has no file or command tools');
      await reloadTimeline(sid);
    } catch (e) {
      toast.error((e as Error).message || 'Could not start the workflow');
    }
  }, [activeSession, sessionMeta, reloadTimeline]);

  const handleSend = useCallback(async (input: string, agentConfigId?: string, projectId?: string, attachments?: AttachmentMeta[]) => {
    if (!wsRef.current) return;
    if (!wsRef.current.isConnected()) {
      toast.error('Connection lost, reconnecting — message not sent');
      return;
    }
    // `/workflow <name> <brief>` starts a workflow into this conversation
    // instead of a turn — the composer's way to what the hub's Run… does.
    if (WORKFLOW_COMMAND.test(input)) {
      await runWorkflowCommand(input.replace(WORKFLOW_COMMAND, ''), agentConfigId, projectId);
      return;
    }
    // `/plan <message>` runs the message in plan mode, `/plan off <message>`
    // leaves it (invariant 33). Handled here, not in the composer: it sets the
    // SESSION's phase, and a new session has no id until the block below makes one.
    const planOff = PLAN_OFF_COMMAND.test(input);
    const planned = !planOff && PLAN_COMMAND.test(input);
    const text = planOff ? input.replace(PLAN_OFF_COMMAND, '') : planned ? input.replace(PLAN_COMMAND, '') : input;
    if ((planned || planOff) && !text.trim()) {
      toast.info(planOff ? '/plan off takes the message to run: /plan off <what to do>' : '/plan takes the message to plan for: /plan <what to do>');
      return;
    }
    // An absent `plan` leaves the session's phase alone — invariant 33.
    const payload: Record<string, unknown> = { session_id: activeSession || PLACEHOLDER_SESSION_ID, input: text, agent_config_id: agentConfigId };
    if (attachments?.length) payload.attachment_ids = attachments.map(a => a.id);
    if (planned) payload.plan = true;
    if (planOff) payload.plan = false;
    if (projectId) payload.project_id = projectId;
    // Over the server's frame limit the socket would be closed (1009), with
    // no run.error to say why — measured before a conversation is made for it.
    if (frameTooLarge(EV.runCreate, payload)) {
      toast.error('Message is too large');
      return;
    }
    // No active session: the first message makes one (invariant 69). It has no
    // history, so it is marked loaded, or the load-session effect would drop
    // the bubble.
    let sid = activeSession;
    let isNew = false;
    if (!sid) {
      try {
        const sess = await api.sessions.create(agentConfigId ? { agent_config_id: agentConfigId } : {}) as { id: string };
        sid = sess.id;
        isNew = true;
        adoptNewSessionPrefs(sid);
        setActiveSession(sid);
        setActivePanel(null);
        setSessionReloadKey(k => k + 1);
      } catch {
        toast.error('Could not start a new session');
        return;
      }
    }
    payload.session_id = sid;
    const clientMsgId = nextClientMsgId();
    updateSS(sid, s => ({ ...s, messages: [...s.messages, { role: 'user', content: text, clientMsgId, attachments }], ...(isNew ? { loaded: true } : {}) }));
    if (!wsRef.current.send(EV.runCreate, payload)) {
      // The socket dropped between the isConnected() check and the send: roll
      // back the optimistic bubble so it isn't left stranded with no run.
      updateSS(sid, s => ({ ...s, messages: s.messages.filter(m => !(m.role === 'user' && m.clientMsgId === clientMsgId)) }));
      toast.error('Connection lost, reconnecting — message not sent');
      return;
    }
  }, [activeSession, updateSS, wsRef, runWorkflowCommand]);

  // handleInject queues a message on the live run — a steer read at its next
  // step, or a follow-up taken when it finishes; what could not be queued goes
  // back to the box.
  const handleInject = useCallback((text: string, queue: InjectQueue): boolean => {
    const sid = activeSession;
    const runId = sid ? ssRef.current[sid]?.liveRunId : null;
    if (!sid || !runId) {
      toast.info('The run just ended — send it as a new message');
      return false;
    }
    const item = { clientMsgId: nextClientMsgId(), runId, text, queue };
    queueInput(sid, item);
    api.runs.inject(runId, { queue, input: text }).catch((e: Error & { status?: number }) => {
      // Only what is still queued comes back here: the run's end returns the rest.
      if (!dropQueued(sid, item.clientMsgId)) return;
      putBackInComposer(sid, activeSessionRef.current === sid, text);
      toast.error(e?.status === 409 ? `Not queued — ${e.message}` : 'Could not queue the message — it is back in the box');
    });
    return true;
  }, [activeSession, queueInput, dropQueued]);

  // handleCancel reports whether the stop was SENT: no live run, or a socket
  // that is down, is a stop that did not happen.
  const handleCancel = useCallback((graceful?: boolean): boolean => {
    if (!wsRef.current || !activeSession) return false;
    const runId = sessionRunRef.current[activeSession];
    if (!runId) return false;
    return wsRef.current.send(EV.runCancel, { run_id: runId, mode: graceful ? 'graceful' : '' });
  }, [activeSession, wsRef, sessionRunRef]);

  const updateToolCall = useCallback((toolCallId: string, patch: ToolCallPatch) => {
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

  // One decision for a whole pause: the cards show approved at once; a
  // refusal puts them back, since the pause is still there to answer.
  const handleApproveAll = useCallback(async (toolCallIds: string[]) => {
    const sid = activeSessionRef.current;
    if (!sid) return;
    for (const id of toolCallIds) updateToolCall(id, { status: 'approved' });
    try {
      await api.sessions.approveAll(sid);
    } catch (e) {
      for (const id of toolCallIds) updateToolCall(id, { status: null });
      toast.error((e as Error).message || 'Could not approve the pending calls');
    }
  }, [updateToolCall]);

  const handleReject = useCallback((toolCallId: string, reason?: string) => {
    if (!wsRef.current) return;
    updateToolCall(toolCallId, { status: 'rejected' });
    if (!wsRef.current.send(EV.toolReject, reason ? { tool_call_id: toolCallId, reason } : { tool_call_id: toolCallId })) {
      updateToolCall(toolCallId, { status: null });
      toast.error('Not connected — rejection not sent, try again');
    }
  }, [updateToolCall, wsRef]);

  const handleDeleteSession = useCallback((deletedId: string) => {
    deleteSession(deletedId);
    clearSessionPrefs(deletedId);
    // The conversation kept beside the hub may be the one deleted: the
    // sidebar only clears the SELECTED one, and the hub shows none as such.
    setActiveSession(prev => (prev === deletedId ? null : prev));
    setSS(prev => {
      if (!prev[deletedId]) return prev;
      const next = { ...prev };
      delete next[deletedId];
      return next;
    });
    // The announced-binding record dies with the session.
    delete announcedBindings.current[deletedId];
    // The deleted session may have carried the last reference to a project —
    // the pickers re-aggregate.
    setBindingsVersion(v => v + 1);
  }, [deleteSession]);

  // The Sessions admin panel deleting or reassigning a conversation away: the
  // same cleanup as the sidebar's delete.
  useEffect(() => {
    const handler = (e: Event) => handleDeleteSession((e as CustomEvent<string>).detail);
    window.addEventListener(SESSION_REMOVED, handler);
    return () => window.removeEventListener(SESSION_REMOVED, handler);
  }, [handleDeleteSession]);

  // A rename from the sidebar: the open conversation's title follows at once
  // (the server announces no rename over the socket).
  const handleRenamed = useCallback((id: string, name: string) => {
    setSessionMeta(prev => (prev && prev.id === id ? { ...prev, name } : prev));
  }, []);

  const handleFork = useCallback(async (messageId: string | number) => {
    if (!activeSession) return;
    try {
      const forked = await api.sessions.fork(activeSession, String(messageId));
      setSessionReloadKey(k => k + 1);
      setActiveSession(forked.id || null);
      setActivePanel(null);
    } catch (e) {
      toast.error((e as Error).message || 'Fork failed');
    }
  }, [activeSession]);

  const handleSwitchBranch = useCallback(async (tipEntryId: string) => {
    if (!activeSession) return;
    try {
      await api.sessions.branch(activeSession, tipEntryId);
      await reloadTimeline(activeSession);
    } catch (e) {
      toast.error((e as Error).message || 'Could not switch attempt');
    }
  }, [activeSession, reloadTimeline]);

  // The Context panel's "Compact now": one forced pass, then a timeline reload
  // (the fold appends a checkpoint no local patch expresses — invariant 24).
  // Toasts carry the outcome, a 409's or 400's message included.
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
    } catch (e) {
      toast.error((e as Error).message || 'Compaction failed');
    }
  }, [activeSession, reloadTimeline]);

  // Regenerating branches back to the user's message and runs again IN PLACE:
  // the attempts live in one session, switchable.
  const handleRegenerate = useCallback(async (userEntryId: string, userContent: string, agentConfigId: string, projectId?: string) => {
    if (!activeSession || !wsRef.current) return;
    // Probe before switching: a regen that branches the session and then fails
    // to send would strand the user on a branch with no assistant reply.
    if (!wsRef.current.isConnected()) {
      toast.error('Connection lost, reconnecting — message not sent');
      return;
    }
    try {
      const { previous_leaf } = await api.sessions.branch(activeSession, userEntryId);
      await reloadTimeline(activeSession);
      // The Inspector stays open: an in-place regen keeps its lens valid. Empty
      // input makes the run answer the branch just switched to, adding no user message.
      const payload: Record<string, unknown> = { session_id: activeSession, input: '', agent_config_id: agentConfigId };
      // A regen can be an unbound session's first project-carrying run — invariant 27.
      if (projectId) payload.project_id = projectId;
      if (!wsRef.current.send(EV.runCreate, payload)) {
        // The socket dropped between the probe and the send: roll the branch
        // back to where it was so the person keeps the attempt they had.
        try {
          await api.sessions.branch(activeSession, previous_leaf);
          await reloadTimeline(activeSession);
          toast.error('Connection lost, reconnecting — regenerate not started');
        } catch {
          toast.error('Connection lost, reconnecting — the previous attempt is in the attempt switcher');
        }
      }
    } catch (e) {
      toast.error((e as Error).message || 'Regenerate failed');
    }
  }, [activeSession, wsRef, reloadTimeline]);

  // A trace row opening its payload: fetched into the active session's state
  // (the panel showing it), from the session whose rows hold the span.
  const handleLoadSpan = useCallback((spanSessionId: string, runId: string, spanId: string): Promise<void> => {
    if (!activeSession) return Promise.resolve();
    return loadSpanPayload(activeSession, spanSessionId, runId, spanId);
  }, [activeSession, loadSpanPayload]);

  // A menu's onSelect hands over an event, which must not become a tab name;
  // narrow is read through a ref so the callback keeps its identity.
  const handleOpenSettings = useCallback((tab?: string) => {
    setSettingsTab(typeof tab === 'string' ? tab : undefined);
    setSettingsOpen(true);
    if (narrowRef.current) setSidebarOpen(false);
  }, []);

  // One object of callbacks, rebuilt only when one of them is; the memo'd
  // view compares it by reference.
  const chatActions = useMemo<ChatViewActions>(() => ({
    onSend: handleSend, onCancel: handleCancel, onApprove: handleApprove, onApproveAll: handleApproveAll, onReject: handleReject, onInject: handleInject, onFork: handleFork,
    onSwitchBranch: handleSwitchBranch, onCompact: handleCompact, onRegenerate: handleRegenerate,
    onWatchTask: watchTask, onUnwatchTask: unwatchTask, onPatchTask: patchTask, onLoadSpan: handleLoadSpan,
    onPanelChange: setActivePanel, onTerminalOpen: handleTerminalOpen, onSettingsOpen: handleOpenSettings, onRetryLoad: handleRetryLoad, onRetryTasks: handleRetryTasks,
  }), [handleSend, handleCancel, handleApprove, handleApproveAll, handleReject, handleInject, handleFork, handleSwitchBranch, handleCompact,
    handleRegenerate, watchTask, unwatchTask, patchTask, handleLoadSpan, handleTerminalOpen, handleOpenSettings, handleRetryLoad, handleRetryTasks]);

  // A signature that moves with any workflow execution in any session (every
  // connection hears every task.updated), for the hub's Runs view to refetch on.
  const tasksSig = useMemo(() => {
    const sig: string[] = [];
    for (const state of Object.values(ss)) {
      for (const t of Object.values(state.tasks)) {
        if (t.kind !== TASK_KIND_WORKFLOW) continue;
        // What the Runs table shows — not updatedAt, which every tool call
        // of a step moves.
        sig.push(t.taskId + ':' + t.status + ':' + (t.attempt || 1) + ':' + (t.state?.step_id || '') + ':' + (t.state?.step_runs?.length || 0));
      }
    }
    return sig.join('|');
  }, [ss]);

  // Stable reference so MemoizedChatView's shallow compare isn't defeated by a
  // fresh object literal every render.
  const sessionBinding = useMemo(() =>
    sessionMeta && sessionMeta.id === activeSession && sessionMeta.projectId
      ? { projectId: sessionMeta.projectId }
      : null,
  [sessionMeta, activeSession]);

  const focusComposer = useCallback(() => {
    setTimeout(() => {
      const el = document.querySelector('.chat-input-box textarea') as HTMLTextAreaElement | null;
      if (el) el.focus();
    }, 0);
  }, []);

  const handleSelectSession = useCallback((id: string | null) => {
    setActiveSession(id);
    setActivePanel(null);
    setHubTab(null);
    if (narrow) setSidebarOpen(false);
  }, [narrow]);

  const handleOpenHub = useCallback(() => {
    setHubTab(tab => tab || 'definitions');
    if (narrow) setSidebarOpen(false);
  }, [narrow]);

  // New, from the sidebar or the rail: an empty composer and no conversation
  // yet — the first message makes one (handleSend) — invariant 69.
  const handleNewSession = useCallback(() => {
    handleSelectSession(null);
    focusComposer();
  }, [handleSelectSession, focusComposer]);

  // A run in the hub opens its session with the execution's detail in the Inspector.
  const handleOpenRun = useCallback((sessionId: string, taskId: string) => {
    setActiveSession(sessionId);
    setActivePanel({ kind: 'task', taskId });
    setHubTab(null);
  }, []);

  if (!authed && checkError) return (
    <ThemeProvider>
      <div className="login-page">
        <div style={{ display: 'flex', flexDirection: 'column', gap: 16, alignItems: 'center' }}>
          <Flash variant="danger">{checkError}</Flash>
          <Button onClick={runCheck}>Retry</Button>
        </div>
      </div>
    </ThemeProvider>
  );
  if (!authed && !checking) return <ThemeProvider><LoginPage onLogin={() => setAuthed(true)} authError={authError} /></ThemeProvider>;
  if (!authed) return <ThemeProvider>{null}</ThemeProvider>;

  const currentSS = ss[activeSession!] || DEFAULT_SS;

  const sidebarPane = (
    <MemoizedSessionList
      activeId={hubTab ? null : activeSession}
      onSelect={handleSelectSession}
      onDelete={handleDeleteSession}
      onRenamed={handleRenamed}
      onNew={handleNewSession}
      reloadKey={sessionReloadKey}
      statuses={sessionStatuses}
      onOpenHub={handleOpenHub}
    />
  );
  const railActions = (
    <>
      <IconButton icon={WorkflowIcon} variant="invisible" aria-label="Workflows" onClick={handleOpenHub} />
      <IconButton icon={PlusIcon} variant="invisible" aria-label="New" onClick={handleNewSession} />
    </>
  );

  const main = hubTab ? (
    <WorkflowsHub tab={hubTab} onTabChange={setHubTab} sessionId={activeSession} tasksSig={tasksSig} onOpenRun={handleOpenRun} />
  ) : (
    <MemoizedChatView
      sessionId={activeSession}
      sessionName={sessionMeta && sessionMeta.id === activeSession ? sessionMeta.name : ''}
      sessionAgentId={sessionMeta && sessionMeta.id === activeSession ? sessionMeta.agentConfigId : undefined}
      sessionBinding={sessionBinding}
      ownPending={!!ownPending && ownPending.id === activeSession && ownPending.pending}
      state={currentSS}
      loadError={currentSS.loadError}
      settingsReloadKey={settingsReloadKey}
      bindingsVersion={bindingsVersion}
      panel={activePanel}
      actions={chatActions}
    />
  );

  return (
    <ThemeProvider>
      <MeContext value={meState}>
        <AttentionSignals announced={sessionStatuses} />
        <AppShell onSettingsOpen={() => handleOpenSettings()} sidebarPane={sidebarPane} railActions={railActions} sidebarOpen={sidebarOpen} onSidebarToggle={setSidebarOpen}>
          {/* A bad turn payload must not take the sidebar, composer and socket
              down with it; switching session or hub tab retries. */}
          <ErrorBoundary resetKey={hubTab ?? activeSession}>{main}</ErrorBoundary>
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
        {/* The sidebar relists on close: the admin panels delete and reassign
            conversations. */}
        {settingsOpen && (
          <PanelDialog title="Settings" tabs={SETTINGS_TABS} adminTabs={isAdmin ? (authMode === 'token' ? ADMIN_TABS.filter(t => t.key !== 'members') : ADMIN_TABS) : undefined} readOnly={isAdmin === null ? null : !isAdmin} initialTab={settingsTab}
            onTabChange={setSettingsTab} onClose={closeSettings} />
        )}
        {/* Lost-connection pill: the socket announces a drop here, not only at
            the moment a send fails. */}
        {!connected && <div className="conn-indicator" role="status">Reconnecting…</div>}
        <GlobalToast />
      </MeContext>
    </ThemeProvider>
  );
}

// Root catches App itself failing to render: the only offer is a reload,
// styled with bare CSS vars since ThemeProvider died with the tree.
export default function Root() {
  return (
    <ErrorBoundary fallback={(_retry, error) => (
      <div style={{ display: 'flex', flexDirection: 'column', alignItems: 'center', gap: 'var(--base-size-16)', marginTop: '20vh', color: 'var(--fgColor-default)' }}>
        <div>The app crashed while rendering: {String(error.message || error)}</div>
        <button onClick={() => window.location.reload()}>Reload</button>
      </div>
    )}>
      <App />
    </ErrorBoundary>
  );
}
