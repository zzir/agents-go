import type { AttachmentMeta } from '@/lib/attachments';
import React, { useState, useCallback, useEffect, useRef, useMemo, memo } from 'react';
import { Flash, Button } from '@primer/react';
import {
  DependabotIcon, McpIcon, ShieldCheckIcon, SparkleIcon, CpuIcon,
  ContainerIcon, DatabaseIcon, FileDirectoryIcon, GearIcon, PersonIcon, PeopleIcon, CommentDiscussionIcon, LogIcon, WorkflowIcon,
} from '@primer/octicons-react';
import { ThemeProvider } from '@/theme/ThemeProvider';
import { AppShell } from '@/layout/AppShell';
import { GlobalToast } from '@/layout/GlobalToast';
import { LoginPage, exchangeErrorTag } from '@/layout/LoginPage';
import { PanelDialog, type DialogTab } from '@/layout/PanelDialog';
import { SessionList as SessionListImpl } from '@/features/sessions/SessionList';
import { ChatView, type ChatViewActions, type InspectorPanel } from '@/features/chat/ChatView';
import { ErrorBoundary } from '@/components/ErrorBoundary';

// Lazy: xterm (+ webgl renderer) is a few hundred KB the first paint never
// needs — the chunk loads when the terminal panel first opens, then the panel
// stays mounted (hidden) so its sessions survive toggles.
const TerminalPanel = React.lazy(() =>
  import('@/features/terminal/TerminalPanel').then(m => ({ default: m.TerminalPanel })),
);
import { checkAuth, getToken, api, exchangeCode, TOKEN_KEY } from '@/lib/api';
import { EV, TASK_KIND_WORKFLOW } from '@/lib/protocol';
import { hasTaskInStatus } from '@/lib/background';
import { WorkflowsHub, type HubTab } from '@/features/workflows/WorkflowsHub';
import { WORKFLOW_COMMAND } from '@/features/chat/SlashMenu';
import { SESSIONS_CHANGED, SESSION_REMOVED } from '@/features/sessions/SessionPicker';
import { useAgentSocket, defaultSS, type SessionState } from '@/lib/useAgentSocket';
import { patchToolCall, type ToolCallPatch } from '@/lib/timeline';
import { syncTaskCard } from '@/lib/streamReducer';
import { clearSessionPrefs } from '@/lib/drafts';
import { toast } from '@/lib/toast';
import { MeContext, useMeLoader } from '@/lib/me';
import { useNarrow } from '@/lib/hooks';
import { readHash, writeHash, consumeAuthFragment, restoreReturnHash } from '@/lib/route';
import { isTooLarge } from '@/lib/messageSize';

// The one settings hub (invariant 61). The person's own first (account,
// host-wide settings), then what a run is built from, each section below the
// ones it depends on: a provider is what an agent talks to, an agent is what
// runs, then what an agent attaches (tools, execution, state, the checks
// around it). A scoped entity's tab is one list in which an admin also sees
// every member's rows. The admin entries come last, under no heading; workflows are
// authored and watched in the sidebar's hub, so only their management view
// is here.
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

// Monotonic client-side id stamped on each optimistic user bubble. It lets the
// socket layer roll back a specific un-sent message (on session_busy or a
// dropped send) and lets the stream reducer dedup two identical-text sends
// without collapsing them into one.
let clientMsgSeq = 0;
function nextClientMsgId(): string { return 'c' + (++clientMsgSeq); }

const MemoizedChatView = memo(ChatView);
// The sidebar re-renders only when a prop moves — the sets below keep their
// identity while their membership holds, so a streaming frame does not redraw
// the whole list.
const MemoizedSessionList = memo(SessionListImpl);

// sameMembers reports whether two sets hold the same ids.
function sameMembers(a: Set<string>, b: Set<string>): boolean {
  if (a.size !== b.size) return false;
  for (const x of a) if (!b.has(x)) return false;
  return true;
}

// hasPendingApproval reports whether a conversation's latest turns hold a tool
// call that needs approval and has no decision yet.
function hasPendingApproval(messages: SessionState['messages']): boolean {
  for (const m of messages) {
    if (m.role !== 'turn') continue;
    for (const part of (m as { parts?: Array<{ type: string; toolCalls?: Array<{ needs_approval?: boolean; status?: string | null }> }> }).parts || []) {
      if (part.type !== 'tools') continue;
      if ((part.toolCalls || []).some(tc => tc.needs_approval && !tc.status)) return true;
    }
  }
  return false;
}

// PLAN_COMMAND is the composer's plan-mode command: a prefix that puts the
// session into plan mode before the request it leads runs. (WORKFLOW_COMMAND,
// the other one, lives with the menu that types it.)
const PLAN_COMMAND = /^\/plan\b[ \t]*/;
// PLAN_OFF_COMMAND leads a message that leaves plan mode: "/plan off <message>".
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
  const [checking, setChecking] = useState(true);
  // The initial auth check failed at the network level (server unreachable), as
  // opposed to resolving "not authenticated". Without this the app would sit on
  // a blank screen forever; instead we surface a retryable error state.
  const [checkError, setCheckError] = useState('');
  const [activeSession, setActiveSession] = useState<string | null>(() => readHash().sessionId);
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
  // The active session's display name and project binding. Captured from the
  // existence-check fetch below and kept fresh by the title_updated /
  // project_bound events; the id guards against a stale response landing after
  // a session switch.
  const [sessionMeta, setSessionMeta] = useState<{ id: string; name: string; projectId: string; agentConfigId: string } | null>(null);
  // Bindings announced over the socket, per session. The session GET races the
  // session.project_bound broadcast (meta is cleared before the fetch, so the
  // event can arrive while prev is null), and a binding is immutable once set
  // — any announced value is THE value, merged over whatever the slower GET
  // returns.
  const announcedBindings = useRef<Record<string, string>>({});
  // Bumped whenever the set of bound sessions changes (a new binding lands, a
  // session is deleted): a bind can auto-create its scratch project, so the
  // project pickers in ChatView and the terminal panel refetch — without this
  // they would only ever show the world as of their mount.
  const [bindingsVersion, setBindingsVersion] = useState(0);
  // Global terminal panel: session-agnostic, opened from the composer (the
  // button only ever opens; closing/collapsing lives on the panel itself).
  // everOpened defers mounting (and the xterm chunk) until first use, after
  // which the panel stays mounted while hidden to keep sessions alive.
  const [terminalOpen, setTerminalOpen] = useState(false);
  const [terminalEverOpened, setTerminalEverOpened] = useState(false);
  // A one-shot "start a terminal for this project" request, set when the
  // composer button opens a CLOSED panel with a project selected (an
  // already-open panel is left alone). The nonce distinguishes repeat requests
  // for the same project.
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
      // A network-level failure (server down, offline) or a non-refusal
      // status (429, 502) rejects here — don't stay stuck in "checking" and
      // don't sign out; show the retry screen below.
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

  // The URL names the view only. A `#/settings/:tab` fragment is a one-shot
  // deep link: opening the dialog re-runs this and writes the view back, so a
  // reload never lands on the overlay with the conversation underneath lost.
  useEffect(() => {
    writeHash(activeSession, activePanel, hubTab);
  }, [activeSession, activePanel, hubTab, settingsOpen]);

  // A lens belongs to a conversation: none open, none shown.
  useEffect(() => {
    if (!activeSession) setActivePanel(null);
  }, [activeSession]);

  useEffect(() => {
    const onHash = () => {
      const { sessionId, panel, hub, settings } = readHash();
      // A settings fragment only opens the dialog; the view underneath stays.
      if (settings != null) {
        setSettingsTab(settings || undefined);
        setSettingsOpen(true);
        return;
      }
      setHubTab(hub);
      if (hub) return; // the conversation beside the hub stays as it was
      setActiveSession(prev => prev === sessionId ? prev : sessionId);
      setActivePanel(prev => panelKey(prev) === panelKey(panel) ? prev : panel);
    };
    window.addEventListener('hashchange', onHash);
    return () => window.removeEventListener('hashchange', onHash);
  }, []);

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

  // A conversation made somewhere other than the sidebar (a picker's "New
  // session") is a conversation the sidebar must list.
  useEffect(() => {
    const handler = () => setSessionReloadKey(k => k + 1);
    window.addEventListener(SESSIONS_CHANGED, handler);
    return () => window.removeEventListener(SESSIONS_CHANGED, handler);
  }, []);

  const updateSS = useCallback((sid: string, fn: (s: SessionState) => SessionState) => {
    setSS(prev => {
      const cur = prev[sid] || defaultSS();
      const next = fn(cur);
      return next === cur ? prev : { ...prev, [sid]: next };
    });
  }, []);

  const { wsRef, sessionRunRef, connected, loadSession, loadTraces, loadSpanPayload, deleteSession, loadEarlier, forgetLoaded, watchTask, unwatchTask } = useAgentSocket(updateSS);

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
        const s = sess as { name?: string; project_id?: string; agent_config_id?: string };
        // A binding announced while this fetch was in flight wins: the fetch
        // read the row before the bind landed, and bindings never change.
        const announced = announcedBindings.current[activeSession];
        setSessionMeta({
          id: activeSession,
          name: s?.name || '',
          projectId: announced || s?.project_id || '',
          // The session's server-side agent: the composer falls back to it when
          // this browser has no local draft (a fork, another device), instead
          // of defaulting to the first agent in the list.
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
  }, [activeSession, loadSession]);

  // Backfill the session's persisted trace summary on load: the chat labels
  // each turn with its run span's duration, and the trace/context lenses join
  // to the same spans. Once per session (loadTraces guards); payloads stay lazy.
  useEffect(() => {
    if (activeSession) loadTraces(activeSession);
  }, [activeSession, loadTraces]);

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
    wsRef.current.on(EV.sessionProjectBound, (p: { session_id?: string; project_id?: string }) => {
      if (p?.session_id && p.project_id) {
        // Record first, then patch the live meta. The record is what makes the
        // announcement survive the meta being null (session switch mid-fetch):
        // the fetch merges it when it lands.
        const projectID = p.project_id;
        announcedBindings.current[p.session_id] = projectID;
        setSessionMeta(prev => (prev && prev.id === p.session_id ? { ...prev, projectId: projectID } : prev));
        setBindingsVersion(v => v + 1);
      }
    });
  }, [wsRef]);

  // reloadTimeline re-reads a session's persisted history after a server-side
  // change the client cannot patch in — a branch move (a different branch is a
  // different conversation), a compaction, a note the server wrote.
  const reloadTimeline = useCallback(async (sid: string) => {
    forgetLoaded(sid);
    await loadSession(sid).catch(() => toast.error('Could not reload conversation'));
  }, [forgetLoaded, loadSession]);

  // runWorkflowCommand is the /workflow command: the first word names the
  // workflow, the rest is its brief. Without a conversation open it makes
  // one, as a message would; the started note the server writes into the
  // conversation is what the reload brings in.
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
        const sess = await api.sessions.create('New Session', agentConfigId) as { id: string };
        sid = sess.id;
        setActiveSession(sid);
        setActivePanel(null);
        setSessionReloadKey(k => k + 1);
      } catch {
        toast.error('Could not start a new session');
        return;
      }
    }
    try {
      // The composer's project rides along, as it does on a message: an
      // unbound conversation is bound to it before the start, so the
      // execution has its file and command tools.
      const body: { session_id: string; input: string; project_id?: string } = { session_id: sid, input: brief };
      if (projectId) body.project_id = projectId;
      await api.workflows.run(wf.id, body);
      toast.success(`Started "${wf.name}" in the background — the result comes back here`);
      // The one thing the person cannot see from here: a conversation with
      // no project — bound or picked — gives the workflow no file or command
      // tools.
      const bound = (sessionMeta && sessionMeta.id === sid ? !!sessionMeta.projectId : false) || !!projectId;
      if (!bound) toast.info('This conversation has no project — the workflow has no file or command tools');
      await reloadTimeline(sid);
    } catch (e) {
      toast.error((e as Error).message || 'Could not start the workflow');
    }
  }, [activeSession, sessionMeta, reloadTimeline]);

  const handleSend = useCallback(async (input: string, agentConfigId?: string, projectId?: string, attachments?: AttachmentMeta[]) => {
    if (!wsRef.current) return;
    if (!wsRef.current.isConnected()) {
      toast.error('WebSocket disconnected — message not sent');
      return;
    }
    // `/workflow <name> <brief>` starts a workflow into this conversation
    // instead of a turn — the composer's way to what the hub's Run… does.
    if (WORKFLOW_COMMAND.test(input)) {
      await runWorkflowCommand(input.replace(WORKFLOW_COMMAND, ''), agentConfigId, projectId);
      return;
    }
    // `/plan <message>` asks for a plan before any change: the message runs
    // in plan mode. It is handled HERE, not in the composer, because it sets
    // the SESSION's phase and a brand-new session has no id until the block
    // below creates one. Nothing arms plan mode ahead of a message — the
    // command IS the message's.
    // `/plan off <message>` is the way out: the message runs with plan mode
    // off, and the session stays out (an approved plan is the other exit).
    const planOff = PLAN_OFF_COMMAND.test(input);
    const planned = !planOff && PLAN_COMMAND.test(input);
    const text = planOff ? input.replace(PLAN_OFF_COMMAND, '') : planned ? input.replace(PLAN_COMMAND, '') : input;
    if ((planned || planOff) && !text.trim()) {
      toast.info(planOff ? '/plan off takes the message to run: /plan off <what to do>' : '/plan takes the message to plan for: /plan <what to do>');
      return;
    }
    // Over the server's frame limit the socket would be closed (1009), with
    // no run.error to say why.
    if (isTooLarge(text)) {
      toast.error('Message is too large');
      return;
    }
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
    updateSS(sid, s => ({ ...s, messages: [...s.messages, { role: 'user', content: text, clientMsgId, attachments }], ...(isNew ? { loaded: true } : {}) }));
    // The phase travels WITH the message: only a /plan message says anything,
    // and an absent `plan` leaves the session's phase alone — an approved plan
    // is what unlocks it again.
    const payload: Record<string, unknown> = { session_id: sid, input: text, agent_config_id: agentConfigId };
    if (attachments?.length) payload.attachment_ids = attachments.map(a => a.id);
    if (planned) payload.plan = true;
    if (planOff) payload.plan = false;
    if (projectId) payload.project_id = projectId;
    if (!wsRef.current.send(EV.runCreate, payload)) {
      // The socket dropped between the isConnected() check and the send: roll
      // back the optimistic bubble so it isn't left stranded with no run.
      updateSS(sid, s => ({ ...s, messages: s.messages.filter((m: { clientMsgId?: string }) => m.clientMsgId !== clientMsgId) }));
      toast.error('WebSocket disconnected — message not sent');
      return;
    }
  }, [activeSession, updateSS, wsRef, runWorkflowCommand]);

  // handleCancel reports whether the stop was SENT: no live run to stop, or a
  // socket that is down, is a stop that did not happen and must not read as
  // one.
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
    // The conversation kept beside the hub may be the one deleted: the
    // sidebar only clears the SELECTED one, and the hub shows none as such.
    setActiveSession(prev => (prev === deletedId ? null : prev));
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

  // The Sessions admin panel deleting or reassigning a conversation away: the
  // same cleanup as the sidebar's delete.
  useEffect(() => {
    const handler = (e: Event) => handleDeleteSession((e as CustomEvent<string>).detail);
    window.addEventListener(SESSION_REMOVED, handler);
    return () => window.removeEventListener(SESSION_REMOVED, handler);
  }, [handleDeleteSession]);

  const handleLoadEarlier = useCallback(() => {
    if (!activeSession) return;
    const s = ssRef.current[activeSession];
    if (!s?.hasMore || s.loadingMore || s.entries.length === 0) return;
    const oldest = s.entries[0]?.id;
    if (oldest) loadEarlier(activeSession, oldest);
  }, [activeSession, loadEarlier]);

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
      toast.error('WebSocket disconnected — message not sent');
      return;
    }
    try {
      const { previous_leaf } = await api.sessions.branch(activeSession, userEntryId);
      await reloadTimeline(activeSession);
      // The Inspector stays open: regen is in-place (same session), so an open
      // trace/task panel remains valid — the replaced attempt gets its "replaced"
      // chip and the drawer follows the new live run.
      // Empty input: the run answers the branch we just switched to rather
      // than adding a new user message. The server maps it to an empty item list.
      const payload: Record<string, unknown> = { session_id: activeSession, input: '', agent_config_id: agentConfigId };
      // A regen can be an unbound session's first project-carrying run, so
      // the project choice rides along; a bound session ignores it anyway.
      if (projectId) payload.project_id = projectId;
      if (!wsRef.current.send(EV.runCreate, payload)) {
        // The socket dropped between the probe and the send: roll the branch
        // back to where it was so the person keeps the attempt they had.
        try {
          await api.sessions.branch(activeSession, previous_leaf);
          await reloadTimeline(activeSession);
          toast.error('WebSocket disconnected — regenerate not started');
        } catch {
          toast.error('WebSocket disconnected — the previous attempt is in the attempt switcher');
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

  // One object of callbacks that change only on a session switch: the memo'd
  // view compares it by reference, so a streaming frame never rebuilds it.
  // The tab is a string or nothing: a menu's onSelect hands over an event,
  // which must not become a tab name. Reads narrow through a ref so the
  // callback keeps its identity and chatActions rebuilds only on a session switch.
  const handleOpenSettings = useCallback((tab?: string) => {
    setSettingsTab(typeof tab === 'string' ? tab : undefined);
    setSettingsOpen(true);
    if (narrowRef.current) setSidebarOpen(false);
  }, []);

  const chatActions = useMemo<ChatViewActions>(() => ({
    onSend: handleSend, onCancel: handleCancel, onApprove: handleApprove, onReject: handleReject, onFork: handleFork,
    onLoadEarlier: handleLoadEarlier, onSwitchBranch: handleSwitchBranch, onCompact: handleCompact, onRegenerate: handleRegenerate,
    onWatchTask: watchTask, onUnwatchTask: unwatchTask, onPatchTask: patchTask, onLoadSpan: handleLoadSpan,
    onPanelChange: setActivePanel, onTerminalOpen: handleTerminalOpen, onSettingsOpen: handleOpenSettings,
  }), [handleSend, handleCancel, handleApprove, handleReject, handleFork, handleLoadEarlier, handleSwitchBranch, handleCompact,
    handleRegenerate, watchTask, unwatchTask, patchTask, handleLoadSpan, handleTerminalOpen, handleOpenSettings]);

  // A signature that moves with any execution in any conversation (every
  // connection hears every session's task.updated), for the hub's Runs view
  // to refetch on.
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

  // Streaming moves ss every animation frame; these two are derived from it
  // but hand out the SAME Set while their membership holds, so the sidebar
  // (memoized on them) redraws on a run starting or ending, not per frame.
  const runningRef = useRef(new Set<string>());
  const runningSessions = useMemo(() => {
    const set = new Set<string>();
    for (const [sid, state] of Object.entries(ss)) {
      if (state.running || hasTaskInStatus(state.tasks, 'working')) set.add(sid);
    }
    if (sameMembers(runningRef.current, set)) return runningRef.current;
    runningRef.current = set;
    return set;
  }, [ss]);

  // Stable reference so MemoizedChatView's shallow compare isn't defeated by a
  // fresh object literal every render.
  const sessionBinding = useMemo(() =>
    sessionMeta && sessionMeta.id === activeSession && sessionMeta.projectId
      ? { projectId: sessionMeta.projectId }
      : null,
  [sessionMeta, activeSession]);

  // A session is awaiting approval when its latest turn holds a tool call that
  // needs approval and has no decision yet, or a background task (a workflow
  // step) is paused for one. Derived from the messages (not a transient socket
  // flag), so it survives a reload — the paused turn is rebuilt from the durable
  // approvals — and self-clears the moment approve/reject sets a status.
  // The scan is per MESSAGE LIST, cached by its identity: a streaming delta
  // replaces the session's streaming text, not its messages, so the frame
  // pays one map lookup per session rather than a walk of every turn.
  const awaitingCache = useRef(new WeakMap<object, boolean>());
  const awaitingRef = useRef(new Set<string>());
  const awaitingSessions = useMemo(() => {
    const set = new Set<string>();
    for (const [sid, state] of Object.entries(ss)) {
      let awaiting = awaitingCache.current.get(state.messages);
      if (awaiting === undefined) {
        awaiting = hasPendingApproval(state.messages);
        awaitingCache.current.set(state.messages, awaiting);
      }
      if (awaiting || hasTaskInStatus(state.tasks, 'input_required')) set.add(sid);
    }
    if (sameMembers(awaitingRef.current, set)) return awaitingRef.current;
    awaitingRef.current = set;
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
    setHubTab(null);
    if (narrow) setSidebarOpen(false);
  }, [narrow]);

  const handleOpenHub = useCallback(() => {
    setHubTab(tab => tab || 'definitions');
    if (narrow) setSidebarOpen(false);
  }, [narrow]);

  // A run in the hub opens its conversation with the execution's detail in
  // the Inspector — the run belongs to that conversation, and the panel there
  // already knows how to show it.
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
      onCreated={handleSessionCreated}
      reloadKey={sessionReloadKey}
      runningSessions={runningSessions}
      awaitingSessions={awaitingSessions}
      onOpenHub={handleOpenHub}
    />
  );

  const main = hubTab ? (
    <WorkflowsHub tab={hubTab} onTabChange={setHubTab} sessionId={activeSession} tasksSig={tasksSig} onOpenRun={handleOpenRun} />
  ) : (
    <MemoizedChatView
      sessionId={activeSession}
      sessionName={sessionMeta && sessionMeta.id === activeSession ? sessionMeta.name : ''}
      sessionAgentId={sessionMeta && sessionMeta.id === activeSession ? sessionMeta.agentConfigId : undefined}
      sessionBinding={sessionBinding}
      state={currentSS}
      awaiting={!!activeSession && awaitingSessions.has(activeSession)}
      settingsReloadKey={settingsReloadKey}
      bindingsVersion={bindingsVersion}
      panel={activePanel}
      actions={chatActions}
    />
  );

  return (
    <ThemeProvider>
      <MeContext value={meState}>
        <AppShell onSettingsOpen={() => handleOpenSettings()} sidebarPane={sidebarPane} sidebarOpen={sidebarOpen} onSidebarToggle={setSidebarOpen}>
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
          <PanelDialog title="Settings" tabs={SETTINGS_TABS} adminTabs={isAdmin ? ADMIN_TABS : undefined} readOnly={isAdmin === null ? null : !isAdmin} initialTab={settingsTab}
            onClose={() => { setSettingsOpen(false); setSettingsTab(undefined); setSettingsReloadKey(k => k + 1); setSessionReloadKey(k => k + 1); }} />
        )}
        {/* Lost-connection pill: the socket announces a drop here, not only at
            the moment a send fails. */}
        {!connected && <div className="conn-indicator" role="status">Reconnecting…</div>}
        <GlobalToast />
      </MeContext>
    </ThemeProvider>
  );
}

// The last line of defense: App itself failed to render. Everything below the
// boundary is gone, so the only honest offer is a reload. Styled with bare
// CSS vars — ThemeProvider died with the tree.
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
