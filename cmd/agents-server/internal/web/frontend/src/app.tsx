import React, { useState, useCallback, useEffect, useRef, useMemo, memo } from 'react';
import { Dialog, NavList as PrimerNavList, Flash, Button, IconButton } from '@primer/react';
import { SecretInput } from '@/components/SecretInput';
import {
  DependabotIcon, McpIcon, ShieldCheckIcon, SparkleIcon, CpuIcon, PlugIcon,
  ContainerIcon, DatabaseIcon, GearIcon, PersonIcon, PeopleIcon, CommentDiscussionIcon, LogIcon, LockIcon,
  XCircleFillIcon, AlertFillIcon, CheckCircleFillIcon, InfoIcon, XIcon,
} from '@primer/octicons-react';
import type { Icon } from '@primer/octicons-react';
import { ThemeProvider } from '@/theme/ThemeProvider';
import { AppShell } from '@/layout/AppShell';
import { SessionList as SessionListImpl } from '@/features/sessions/SessionList';
import { ChatView, type ChatViewActions, type InspectorPanel } from '@/features/chat/ChatView';
import { ErrorBoundary } from '@/components/ErrorBoundary';

// Lazy: xterm (+ webgl renderer) is a few hundred KB the first paint never
// needs — the chunk loads when the terminal panel first opens, then the panel
// stays mounted (hidden) so its sessions survive toggles.
const TerminalPanel = React.lazy(() =>
  import('@/features/terminal/TerminalPanel').then(m => ({ default: m.TerminalPanel })),
);
import { login, checkAuth, getToken, api, authConfig, exchangeCode, TOKEN_KEY, type AuthConfig } from '@/lib/api';
import { EV, TASK_KIND_WORKFLOW } from '@/lib/protocol';
import { WorkflowsHub, type HubTab } from '@/features/workflows/WorkflowsHub';
import { WORKFLOW_COMMAND } from '@/features/chat/SlashMenu';
import { SESSIONS_CHANGED, SESSION_REMOVED } from '@/features/sessions/SessionPicker';
import { useAgentSocket, defaultSS, type SessionState } from '@/lib/useAgentSocket';
import { patchToolCall, type ToolCallPatch } from '@/lib/timeline';
import { syncTaskCard } from '@/lib/streamReducer';
import { clearSessionPrefs } from '@/lib/drafts';
import { onToast, toast } from '@/lib/toast';
import { ReadOnlyContext } from '@/lib/access';
import { MeContext, useMeLoader } from '@/lib/me';
import { useNarrow } from '@/lib/hooks';
import { readHash, writeHash, consumeAuthFragment, stashReturnHash, restoreReturnHash } from '@/lib/route';
import { isTooLarge } from '@/lib/messageSize';

const FLASH_VARIANT: Record<string, FlashProps['variant']> = { error: 'danger', warning: 'warning', success: 'success', info: 'default' };
const FLASH_ICON: Record<string, React.ReactNode> = {
  error: <XCircleFillIcon size={16} />,
  warning: <AlertFillIcon size={16} />,
  success: <CheckCircleFillIcon size={16} />,
  info: <InfoIcon size={16} />,
};
type FlashProps = React.ComponentProps<typeof Flash>;

// A queue, not one slot: three errors during a long run stack up instead of
// each overwriting the last. Errors stay until dismissed — a click, their
// close button, or Escape (the newest first); everything else auto-dismisses.
// The stack div always exists so the live region is established before the
// first announcement.
function GlobalToast() {
  const [items, setItems] = useState<Array<{ id: number; msg: string; type: string; exiting?: boolean }>>([]);
  const seqRef = useRef(0);
  const timersRef = useRef<Map<number, ReturnType<typeof setTimeout>>>(new Map());

  const dismiss = useCallback((id: number) => {
    const t = timersRef.current.get(id);
    if (t) { clearTimeout(t); timersRef.current.delete(id); }
    setItems(prev => prev.map(it => (it.id === id ? { ...it, exiting: true } : it)));
    setTimeout(() => setItems(prev => prev.filter(it => it.id !== id)), 150);
  }, []);

  useEffect(() => {
    onToast(({ msg, type }) => {
      const id = ++seqRef.current;
      setItems(prev => [...prev.slice(-4), { id, msg, type }]);
      if (type !== 'error') timersRef.current.set(id, setTimeout(() => dismiss(id), 4000));
    });
    const timers = timersRef.current;
    return () => {
      onToast(null);
      for (const t of timers.values()) clearTimeout(t);
    };
  }, [dismiss]);

  const itemsRef = useRef(items);
  itemsRef.current = items;
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape' || e.defaultPrevented) return;
      const last = [...itemsRef.current].reverse().find(it => !it.exiting);
      if (last) dismiss(last.id);
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [dismiss]);

  return (
    <div className="global-toast-stack" role="status" aria-live="polite">
      {items.map(it => (
        <Flash
          key={it.id}
          variant={FLASH_VARIANT[it.type] || 'default'}
          role={it.type === 'error' ? 'alert' : undefined}
          className={'global-toast' + (it.exiting ? ' global-toast-exit' : '')}
          onClick={() => dismiss(it.id)}
        >
          <span style={{ display: 'inline-flex', alignItems: 'center', gap: 8 }}>
            {FLASH_ICON[it.type]}{it.msg}
          </span>
          {it.type === 'error' && (
            <IconButton icon={XIcon} size="small" variant="invisible" aria-label="Dismiss" className="global-toast-close"
              onClick={e => { e.stopPropagation(); dismiss(it.id); }} />
          )}
        </Flash>
      ))}
    </div>
  );
}

type DialogTab = { key: string; label: string; icon: Icon; load: () => Promise<{ default: React.ComponentType }>; dividerBefore?: boolean };

// Ordered so each section builds on the ones above it: a provider is what an
// agent talks to, an agent is what runs, then what an agent attaches (tools,
// execution, state, the checks around it). General last. Workflows are not
// here: they are authored once and then WATCHED, which is the sidebar's
// Workflows hub, not a settings tab.
const SETTINGS_TABS: DialogTab[] = [
  { key: 'providers',  label: 'Providers',  icon: CpuIcon,        load: () => import('@/features/providers/ProviderPanel') },
  { key: 'agents',     label: 'Agents',     icon: DependabotIcon, load: () => import('@/features/agents/AgentConfigPanel') },
  { key: 'mcp',        label: 'MCP',        icon: McpIcon,        load: () => import('@/features/mcp/McpServerPanel') },
  { key: 'skills',     label: 'Skills',     icon: SparkleIcon,    load: () => import('@/features/skills/SkillsPanel') },
  { key: 'sandbox',    label: 'Sandbox',    icon: ContainerIcon,  load: () => import('@/features/sandbox/SandboxPanel') },
  { key: 'memory',     label: 'Memory',     icon: DatabaseIcon,   load: () => import('@/features/memory/MemoryPanel') },
  { key: 'guardrails', label: 'Guardrails', icon: ShieldCheckIcon, load: () => import('@/features/guardrails/GuardrailPanel') },
  { key: 'plugins',    label: 'Plugins',    icon: PlugIcon,       load: () => import('@/features/plugins/PluginsPanel') },
  // Shared configuration above the line, the person's own below it.
  { key: 'account',    label: 'Account',    icon: PersonIcon,     load: () => import('@/features/account/AccountPanel'), dividerBefore: true },
  { key: 'general',    label: 'General',    icon: GearIcon,       load: () => import('@/features/settings/SettingsPanel') },
];


// The Admin dialog: people and what they own, then the record of it all.
const ADMIN_TABS: DialogTab[] = [
  { key: 'members',  label: 'Members',    icon: PeopleIcon,            load: () => import('@/features/admin/MembersPanel') },
  { key: 'sessions', label: 'Sessions',   icon: CommentDiscussionIcon, load: () => import('@/features/admin/SessionsPanel') },
  { key: 'audit',    label: 'Audit logs', icon: LogIcon,               load: () => import('@/features/admin/AuditPanel') },
];

function TabLoadError() {
  return <Flash variant="danger">Failed to load this panel — reload the page.</Flash>;
}

// The scoped panels: rows carry their own scope/owner, so a member writes
// there too — the dialog's blanket read-only applies only to the other tabs.
const SCOPED_TABS = new Set(['providers', 'agents', 'mcp', 'skills']);

// PanelDialog is the tabbed dialog behind both Settings and Admin: a nav of
// lazily loaded panels, one open at a time. readOnly is a member's Settings:
// shared configuration is theirs to read (the API allows it) and not to
// write (the server refuses with 403), so the panels show and offer nothing.
// readOnly null is "not known yet" (/auth/me still loading): the nav shows,
// the panel waits, so an admin never sees the read-only note flash.
function PanelDialog({ title, tabs, readOnly, onClose }: { title: string; tabs: DialogTab[]; readOnly?: boolean | null; onClose: () => void }) {
  const [tab, setTab] = useState(tabs[0].key);
  const [TabComp, setTabComp] = useState<React.ComponentType | null>(null);
  const narrow = useNarrow();

  useEffect(() => {
    // The previous panel stays on screen while the next chunk loads — clearing
    // first blanked the dialog on every switch, even for already-cached
    // modules (the import still resolves a microtask later). The stale flag
    // drops a resolution that no longer matches the selected tab: a slow
    // first-load chunk must not overwrite a faster later click's panel.
    let stale = false;
    const entry = tabs.find(t => t.key === tab);
    if (!entry) return;
    entry.load().then(mod => {
      if (!stale) setTabComp(() => mod.default);
    }).catch(() => {
      // A stale chunk 404 would otherwise leave the panel blank forever.
      if (!stale) setTabComp(() => TabLoadError);
    });
    return () => { stale = true; };
  }, [tab, tabs]);

  return (
    <Dialog
      title={title}
      onClose={() => onClose()}
      height="auto"
      position={{ narrow: 'fullscreen', regular: 'center' }}
      // Both sides scale with the viewport and cap, so the dialog stays a
      // landscape box on a large screen instead of a column (a capped width
      // under an uncapped height); Primer's own max-* still clamp small ones.
      // The width cap is the nav plus the 1100px content column plus margins
      // — wider only adds blank space. A narrow screen takes Primer's
      // fullscreen instead.
      style={narrow ? undefined : { width: 'clamp(960px, 80dvw, 1360px)', height: 'clamp(560px, 85dvh, 1000px)' }}
      renderBody={({ children }) => (
        <Dialog.Body className="settings-body" style={{ padding: 0 }}>
          {children}
        </Dialog.Body>
      )}
    >
      <div className="settings-layout">
        <nav className="settings-nav">
          <PrimerNavList aria-label={`${title} sections`}>
            {tabs.map(t => (
              <React.Fragment key={t.key}>
                {t.dividerBefore && tabs[0] !== t && <PrimerNavList.Divider />}
                <PrimerNavList.Item
                  aria-current={tab === t.key ? 'page' : undefined}
                  onClick={() => setTab(t.key)}
                >
                  <PrimerNavList.LeadingVisual><t.icon size={16} /></PrimerNavList.LeadingVisual>
                  {t.label}
                </PrimerNavList.Item>
              </React.Fragment>
            ))}
          </PrimerNavList>
        </nav>
        <div className="settings-content">
          {/* The why behind the read-only panels — once, above whichever
              admin-written panel is open; Account is the member's own, and
              the scoped panels take member writes. */}
          {readOnly && tab !== 'account' && !SCOPED_TABS.has(tab) && (
            <Flash variant="default" className="settings-readonly-note">
              <span style={{ display: 'inline-flex', alignItems: 'center', gap: 8 }}>
                <LockIcon size={16} />
                Read-only. Shared configuration is managed by admins; you can use all of it in your own sessions.
              </span>
            </Flash>
          )}
          {TabComp && readOnly !== null ? (
            // Scoped panels are excluded from the blanket: they gate per row
            // (canEditRow) and set the context themselves around their form.
            <ReadOnlyContext value={!!readOnly && !SCOPED_TABS.has(tab)}>
              <ErrorBoundary resetKey={tab}><TabComp /></ErrorBoundary>
            </ReadOnlyContext>
          ) : null}
        </div>
      </div>
    </Dialog>
  );
}

// AUTH_ERROR_TEXT maps the callback's coarse #auth_error tags to a sentence.
const AUTH_ERROR_TEXT: Record<string, string> = {
  state_mismatch: 'The sign-in expired or was already used — try again.',
  exchange_failed: 'The provider rejected the sign-in — try again.',
  not_allowed: 'This account is not on the allowlist for this server.',
  cancelled: 'The sign-in was cancelled at the provider.',
  disabled: 'This account has been disabled by an admin.',
  rate_limited: 'Too many sign-in attempts from your address — wait a minute and try again.',
  login_failed: 'Sign-in failed on the server — try again.',
};

// exchangeErrorTag maps a failed code exchange to the login page's message:
// the server refuses a used or expired code with 401; anything else is not
// the code's fault.
function exchangeErrorTag(e: unknown): string {
  const status = (e as { status?: number } | null)?.status;
  if (status === 401) return 'state_mismatch';
  if (status === 429) return 'rate_limited';
  return 'login_failed';
}

function LoginPage({ onLogin, authError }: { onLogin: () => void; authError?: string }) {
  const [token, setTokenVal] = useState('');
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(false);
  // null while /auth/config is in flight. A failure shows as such, with a
  // retry — guessing token mode would offer a password box that an OAuth
  // server answers with 400.
  const [cfg, setCfg] = useState<AuthConfig | null>(null);
  const [cfgError, setCfgError] = useState(false);
  const [cfgAttempt, setCfgAttempt] = useState(0);

  useEffect(() => {
    let stale = false;
    setCfgError(false);
    authConfig()
      .then(c => { if (!stale) setCfg(c); })
      .catch(() => { if (!stale) setCfgError(true); });
    return () => { stale = true; };
  }, [cfgAttempt]);

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

  const oauthMsg = authError ? (AUTH_ERROR_TEXT[authError] || AUTH_ERROR_TEXT.login_failed) : '';

  return (
    <div className="login-page">
      <form className="login-card" onSubmit={handleSubmit}>
        {oauthMsg ? <Flash variant="danger">{oauthMsg}</Flash> : null}
        {cfgError ? (
          <>
            <Flash variant="danger">Couldn&apos;t load the sign-in options from the server.</Flash>
            <Button block onClick={() => setCfgAttempt(n => n + 1)}>Retry</Button>
          </>
        ) : cfg?.mode === 'oauth' ? (
          (cfg.providers || []).map(p => (
            // A full-page navigation: the flow returns via the server's
            // redirect with a one-time code in the fragment.
            <Button
              key={p} block variant="primary"
              onClick={() => { stashReturnHash(); window.location.href = `/api/v1/auth/oauth/${p}/start`; }}
            >
              Sign in with {p.charAt(0).toUpperCase() + p.slice(1)}
            </Button>
          ))
        ) : cfg ? (
          <>
            <SecretInput
              aria-label="API token"
              placeholder="Token"
              value={token}
              autoFocus
              loading={loading || undefined}
              onChange={(e) => setTokenVal(e.target.value)}
              validationStatus={error ? 'error' : undefined}
            />
            <Button type="submit" variant="primary" block disabled={loading || !token.trim()}>Sign in</Button>
          </>
        ) : null}
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
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [adminOpen, setAdminOpen] = useState(false);
  const [sidebarOpen, setSidebarOpen] = useState(false);
  const [sessionReloadKey, setSessionReloadKey] = useState(0);
  // Bumped by workflow.updated: the chat's workflow strip and Tasks panel
  // refetch when a background sequence moves.
  const [settingsReloadKey, setSettingsReloadKey] = useState(0);
  // The active session's display name and sandbox binding. Captured from the
  // existence-check fetch below and kept fresh by the title_updated /
  // sandbox_bound events; the id guards against a stale response landing after
  // a session switch.
  const [sessionMeta, setSessionMeta] = useState<{ id: string; name: string; sandboxId: string; projectId: string } | null>(null);
  // Bindings announced over the socket, per session. The session GET races the
  // session.sandbox_bound broadcast (meta is cleared before the fetch, so the
  // event can arrive while prev is null), and a binding is immutable once set
  // — any announced value is THE value, merged over whatever the slower GET
  // returns.
  const announcedBindings = useRef<Record<string, { sandboxId: string; projectId: string }>>({});
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
  // A one-shot "start a terminal for this sandbox" request, set when the
  // composer button opens a CLOSED panel with a capable sandbox selected (an
  // already-open panel is left alone). The nonce distinguishes repeat requests
  // for the same sandbox.
  const [terminalRequest, setTerminalRequest] = useState<{ id: string; name: string; projectId: string; projectName?: string; nonce: number } | null>(null);
  const terminalOpenRef = useRef(false);
  terminalOpenRef.current = terminalOpen;
  const terminalNonceRef = useRef(0);
  const handleTerminalOpen = useCallback((sandbox?: { id: string; name: string; projectId: string; projectName?: string }) => {
    if (!terminalOpenRef.current && sandbox) {
      setTerminalRequest({ ...sandbox, nonce: ++terminalNonceRef.current });
    }
    setTerminalOpen(true);
    setTerminalEverOpened(true);
  }, []);

  const [ss, setSS] = useState<Record<string, SessionState>>({});

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

  useEffect(() => {
    writeHash(activeSession, activePanel, hubTab);
  }, [activeSession, activePanel, hubTab]);

  useEffect(() => {
    const onHash = () => {
      const { sessionId, panel, hub } = readHash();
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
        const s = sess as { name?: string; sandbox_id?: string; project_id?: string };
        // A binding announced while this fetch was in flight wins: the fetch
        // read the row before the bind landed, and bindings never change.
        const announced = announcedBindings.current[activeSession];
        setSessionMeta({
          id: activeSession,
          name: s?.name || '',
          sandboxId: announced ? announced.sandboxId : (s?.sandbox_id || ''),
          projectId: announced ? announced.projectId : (s?.project_id || ''),
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
    wsRef.current.on(EV.sessionSandboxBound, (p: { session_id?: string; sandbox_id?: string; project_id?: string }) => {
      if (p?.session_id && p.sandbox_id) {
        // Record first, then patch the live meta. The record is what makes the
        // announcement survive the meta being null (session switch mid-fetch):
        // the fetch merges it when it lands.
        announcedBindings.current[p.session_id] = { sandboxId: p.sandbox_id, projectId: p.project_id || '' };
        setSessionMeta(prev => (prev && prev.id === p.session_id
          ? { ...prev, sandboxId: p.sandbox_id!, projectId: p.project_id || '' }
          : prev));
        // The bind may have created its scratch project: refresh the pickers.
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
  const runWorkflowCommand = useCallback(async (rest: string, agentConfigId?: string, sandboxId?: string, projectId?: string) => {
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
      const body: { session_id: string; input: string; sandbox_id?: string; project_id?: string } = { session_id: sid, input: brief };
      if (sandboxId) {
        body.sandbox_id = sandboxId;
        if (projectId) body.project_id = projectId;
      }
      await api.workflows.run(wf.id, body);
      toast.success(`Started "${wf.name}" in the background — the result comes back here`);
      // The one thing the person cannot see from here: a conversation with
      // no project — bound or picked — gives the workflow no file or command
      // tools.
      const bound = (sessionMeta && sessionMeta.id === sid ? !!sessionMeta.sandboxId : false) || !!sandboxId;
      if (!bound) toast.info('This conversation has no project — the workflow has no file or command tools');
      await reloadTimeline(sid);
    } catch (e) {
      toast.error((e as Error).message || 'Could not start the workflow');
    }
  }, [activeSession, sessionMeta, reloadTimeline]);

  const handleSend = useCallback(async (input: string, agentConfigId?: string, sandboxId?: string, projectId?: string) => {
    if (!wsRef.current) return;
    if (!wsRef.current.isConnected()) {
      toast.error('WebSocket disconnected — message not sent');
      return;
    }
    // `/workflow <name> <brief>` starts a workflow into this conversation
    // instead of a turn — the composer's way to what the hub's Run… does.
    if (WORKFLOW_COMMAND.test(input)) {
      await runWorkflowCommand(input.replace(WORKFLOW_COMMAND, ''), agentConfigId, sandboxId, projectId);
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
    updateSS(sid, s => ({ ...s, messages: [...s.messages, { role: 'user', content: text, clientMsgId }], ...(isNew ? { loaded: true } : {}) }));
    // The phase travels WITH the message: only a /plan message says anything,
    // and an absent `plan` leaves the session's phase alone — an approved plan
    // is what unlocks it again.
    const payload: Record<string, unknown> = { session_id: sid, input: text, agent_config_id: agentConfigId };
    if (planned) payload.plan = true;
    if (planOff) payload.plan = false;
    if (sandboxId) {
      payload.sandbox_id = sandboxId;
      if (projectId) payload.project_id = projectId;
    }
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

  // The Admin dialog deleting or reassigning a conversation away: the same
  // cleanup as the sidebar's delete.
  useEffect(() => {
    const handler = (e: Event) => handleDeleteSession((e as CustomEvent<string>).detail);
    window.addEventListener(SESSION_REMOVED, handler);
    return () => window.removeEventListener(SESSION_REMOVED, handler);
  }, [handleDeleteSession]);

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

  // Regenerating branches back to the user's message and runs again IN PLACE.
  // It used to fork a whole new session per attempt, which is why a chat list
  // filled up with "(regen 2)", "(regen 3)" and no way to compare them — the
  // attempts now live in one session, switchable.
  const handleRegenerate = useCallback(async (userEntryId: string, userContent: string, agentConfigId: string, sandboxId: string, projectId?: string) => {
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
      const payload: Record<string, unknown> = { session_id: activeSession, input: '', agent_config_id: agentConfigId };
      if (sandboxId) {
        payload.sandbox_id = sandboxId;
        // A regen can be an unbound session's first sandbox-carrying run, so
        // the project choice rides along; a bound session ignores it anyway.
        if (projectId) payload.project_id = projectId;
      }
      if (!wsRef.current.send(EV.runCreate, payload)) {
        toast.error('WebSocket disconnected — message not sent');
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

  // One object of stable callbacks: the memo'd view compares it by reference.
  const chatActions = useMemo<ChatViewActions>(() => ({
    onSend: handleSend, onCancel: handleCancel, onApprove: handleApprove, onReject: handleReject, onFork: handleFork,
    onLoadEarlier: handleLoadEarlier, onSwitchBranch: handleSwitchBranch, onCompact: handleCompact, onRegenerate: handleRegenerate,
    onWatchTask: watchTask, onUnwatchTask: unwatchTask, onPatchTask: patchTask, onLoadSpan: handleLoadSpan,
    onPanelChange: setActivePanel, onTerminalOpen: handleTerminalOpen,
  }), [handleSend, handleCancel, handleApprove, handleReject, handleFork, handleLoadEarlier, handleSwitchBranch, handleCompact,
    handleRegenerate, watchTask, unwatchTask, patchTask, handleLoadSpan, handleTerminalOpen]);

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
      if (state.running) set.add(sid);
    }
    if (sameMembers(runningRef.current, set)) return runningRef.current;
    runningRef.current = set;
    return set;
  }, [ss]);

  // Stable reference so MemoizedChatView's shallow compare isn't defeated by a
  // fresh object literal every render.
  const sessionBinding = useMemo(() =>
    sessionMeta && sessionMeta.id === activeSession && sessionMeta.sandboxId
      ? { sandboxId: sessionMeta.sandboxId, projectId: sessionMeta.projectId }
      : null,
  [sessionMeta, activeSession]);

  // A session is awaiting approval when its latest turn holds a tool call that
  // needs approval and has no decision yet. Derived from the messages (not a
  // transient socket flag), so it survives a reload — the paused turn is rebuilt
  // from the durable approvals — and self-clears the moment approve/reject sets
  // a status.
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
      if (awaiting) set.add(sid);
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
    if (window.innerWidth < 768) setSidebarOpen(false);
  }, []);

  const handleOpenHub = useCallback(() => {
    setHubTab(tab => tab || 'definitions');
    if (window.innerWidth < 768) setSidebarOpen(false);
  }, []);

  // A run in the hub opens its conversation with the execution's detail in
  // the Inspector — the run belongs to that conversation, and the panel there
  // already knows how to show it.
  const handleOpenRun = useCallback((sessionId: string, taskId: string) => {
    setActiveSession(sessionId);
    setActivePanel({ kind: 'task', taskId });
    setHubTab(null);
  }, []);

  // The Plugins placeholder is an admin's preview of the settings' shape.
  // Memoized: the dialog reloads its panel whenever the tab list changes.
  const settingsTabs = useMemo(() => (isAdmin ? SETTINGS_TABS : SETTINGS_TABS.filter(t => t.key !== 'plugins')), [isAdmin]);

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
        <AppShell onSettingsOpen={() => setSettingsOpen(true)} onAdminOpen={() => setAdminOpen(true)} sidebarPane={sidebarPane} sidebarOpen={sidebarOpen} onSidebarToggle={setSidebarOpen}>
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
        {settingsOpen && (
          <PanelDialog title="Settings" tabs={settingsTabs} readOnly={isAdmin === null ? null : !isAdmin}
            onClose={() => { setSettingsOpen(false); setSettingsReloadKey(k => k + 1); }} />
        )}
        {/* Admin deletes sessions; the sidebar relists on close. */}
        {adminOpen && <PanelDialog title="Admin" tabs={ADMIN_TABS} onClose={() => { setAdminOpen(false); setSessionReloadKey(k => k + 1); }} />}
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
