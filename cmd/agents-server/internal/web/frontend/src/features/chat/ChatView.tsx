import './chat.css';
import { useState, useEffect, useCallback, useMemo, useRef, type MouseEvent, type ReactNode } from 'react';
import { Button, Dialog, IconButton, ActionMenu, ActionList, Select, Stack, TextInput, useConfirm } from '@primer/react';
import { Blankslate } from '@primer/react/experimental';
import { api } from '@/lib/api';
import { CHECK_ICON } from '@/lib/markdownShared';
import { type TurnPart, type TimelineEntry, type Branches, type WorkflowStartedNote } from '@/lib/timeline';
import { useScrollToBottom, useApi } from '@/lib/hooks';
import { loadSessionAgent, saveSessionAgent, loadLastAgent, saveLastAgent, loadSessionProject, saveSessionProject } from '@/lib/drafts';
import { composerSandboxView, groupProjects, projectLabel, type EnvVar, type Project, type SandboxSupports, type SessionBinding } from '@/lib/binding';
import { useProjects } from '@/lib/useProjects';
import { fc } from '@/lib/form';
import { parseTaskNotification, TASK_KIND_WORKFLOW, type TaskStatus } from '@/lib/protocol';
import type { SessionState, TaskState } from '@/lib/useAgentSocket';
import { ChatSessionProvider, useDerivedChatTasks, type ChatSessionState, type ChatActions } from '@/features/chat/ChatSessionContext';
import { BackgroundListPanel, BackgroundDetailPanel, BackgroundMissingPanel } from '@/features/chat/BackgroundPanel';
import { AgentAvatar } from '@/components/AgentAvatar';
import { MessageBubble } from '@/features/chat/MessageBubble';
import { TurnBlock } from '@/features/chat/TurnBlock';
import { UserMessage } from '@/features/chat/UserMessage';
import { WorkflowStartedChip, originText } from '@/features/chat/WorkflowStartedChip';
import { CompactionCard } from '@/features/chat/CompactionCard';
import { Greeting } from '@/features/chat/Greeting';
import { ChatToc } from '@/features/chat/ChatToc';
import { MessageInput } from '@/features/chat/MessageInput';
import { WorkflowStrip } from '@/features/chat/WorkflowStrip';
import { TraceDrawer, type TraceReveal } from '@/features/chat/TracePanel';
import { ContextPanel } from '@/features/chat/ContextPanel';
import { ChatTopBar } from '@/features/chat/ChatTopBar';
import { ProjectEnvDialog, parsePorts } from '@/features/chat/ProjectEnvDialog';
import { EnvEditor, cleanEnv, envError } from '@/components/EnvEditor';
import { ArrowDownIcon, CommentDiscussionIcon, FileDirectoryIcon, PlusIcon } from '@primer/octicons-react';
import { toast } from '@/lib/toast';

/* ---------- types ---------- */

// `task` is the detail lens over ONE piece of background work — taskId is a
// task id or a workflow-execution id, whichever the list row was.
export type InspectorPanel = null | { kind: 'trace' } | { kind: 'tasks' } | { kind: 'task'; taskId: string } | { kind: 'context' };

interface ChatMessage {
  role: string;
  content?: string;
  // The run that produced this row — what groups a workflow's turns together.
  runId?: string;
  messageId?: string;
  // The durable entry id — what a branch switch and a regenerate aim at.
  entryId?: string;
  parts?: TurnPart[];
  // Present on a turn that is one of several attempts at the same point.
  branches?: Branches;
  // Present on a compaction checkpoint: the entries it folded away, and the
  // context size on either side of the pass.
  folded?: TimelineEntry[];
  tokensBefore?: number;
  tokensAfter?: number;
  // Present on a workflow-started note (a system row).
  note?: WorkflowStartedNote;
}

// Stable React key for a rendered message: prefer the durable store id, then
// the run id or the sender's optimistic client id, and only fall back to the
// array index for a transient entry that has none. Type-tagged prefixes
// (m/r/c/i) keep the id and index number-spaces from colliding; the role
// prefix keeps a user bubble and a turn that share a run id distinct. Plain
// index keys let collapse / copied state drift onto the wrong message whenever
// the list length changed (reload, fork, session switch).
function entryKey(
  m: { messageId?: string | number; runId?: string; clientMsgId?: string },
  i: number,
  role: string,
): string {
  if (m.messageId != null) return role + '-m' + m.messageId;
  if (m.runId) return role + '-r' + m.runId;
  if (m.clientMsgId) return role + '-c' + m.clientMsgId;
  return role + '-i' + i;
}


interface AgentConfig {
  id: string;
  name: string;
  avatar?: string;
}

interface SandboxDef {
  id: string;
  name: string;
  /* Capabilities as the API row declares them — never sniffed from a type. */
  supports?: SandboxSupports;
}

// Restartable jump-target flash, shared by trace reverse-navigation and the
// TOC rail.
function flashMessage(el: Element) {
  el.classList.remove('msg-jump-flash');
  // Restart the animation even when jumping to the same message twice.
  void (el as HTMLElement).offsetWidth;
  el.classList.add('msg-jump-flash');
  window.setTimeout(() => el.classList.remove('msg-jump-flash'), 1800);
}

/* ---------- ChatView ---------- */

// ChatViewActions is what the view can ask the app to do. Every member is a
// stable callback, so the object is memoized once and the memo'd view holds.
export interface ChatViewActions {
  onSend: (text: string, agentConfigId: string, projectId?: string) => void;
  onCancel: (graceful?: boolean) => boolean;
  onApprove?: (id: string, scope?: string) => void;
  onReject?: (id: string) => void;
  onFork?: (id: string) => void;
  // Backwards pagination over the persisted history (state.hasMore says
  // older entries exist).
  onLoadEarlier?: () => void;
  // Switches the session's active branch to another attempt.
  onSwitchBranch?: (tipEntryId: string) => void;
  // Forces one compaction pass now (the Context panel's button); resolves
  // after the timeline reload that follows a fold.
  onCompact?: () => Promise<void>;
  onRegenerate?: (userEntryId: string, userContent: string, agentConfigId: string, projectId?: string) => void;
  onWatchTask?: (sid: string, taskId: string, childSessionId: string) => void;
  onUnwatchTask?: (sid: string) => void;
  // Applies a server-confirmed task state change (the stop API response) —
  // the fallback when no hub broadcast will come (paused task after restart).
  onPatchTask?: (sid: string, taskId: string, patch: Partial<TaskState>) => void;
  // Loads one trace span's payload (left out of the listing) into the panel
  // showing it, from the session whose stored rows hold the span.
  onLoadSpan?: (spanSessionId: string, runId: string, spanId: string) => Promise<void>;
  onPanelChange: (panel: InspectorPanel) => void;
  // Opens the global terminal panel (app-level, independent of the session).
  // Open-only by design: closing/collapsing happens on the panel itself. When
  // the session is bound its project is passed along, and a freshly opened
  // panel starts a terminal for it — in the same container the session's runs
  // use.
  onTerminalOpen?: (project?: { projectId: string; projectName?: string; targetName?: string }) => void;
}

interface ChatViewProps {
  sessionId: string | null;
  // The session's display name for the top bar ('' until known).
  sessionName?: string;
  // The session's server-side agent binding. `undefined` means "not loaded
  // yet"; the composer's agent falls back to it (before the first agent in the
  // list) when this browser holds no local draft — e.g. a fork or another
  // device. '' is a resolved session with no agent bound.
  sessionAgentId?: string;
  // The session's permanent project binding, or null while unbound. Set by
  // the first project-carrying run; server-authoritative and immutable
  // afterwards — switching projects means starting a new session.
  sessionBinding?: SessionBinding | null;
  // The session as the socket layer keeps it: timeline, stream, live run,
  // tasks, history paging. One reference per session, replaced on change.
  state: SessionState;
  // The session is paused awaiting a tool approval: block new sends so the
  // approval is resolved first (a concurrent run would strand it as session_busy).
  awaiting?: boolean;
  settingsReloadKey?: number;
  // Bumped by the app when the set of session bindings changed; refreshes the
  // Project picker's list.
  bindingsVersion?: number;
  panel: InspectorPanel;
  actions: ChatViewActions;
}

export function ChatView({
  sessionId, sessionName, sessionAgentId, sessionBinding, state, awaiting, settingsReloadKey, bindingsVersion, panel, actions,
}: ChatViewProps) {
  // The rendered timeline drops the entries no longer on the active branch;
  // the trace panel still lists their runs, so it reads the raw entries.
  const messages: ChatMessage[] = state.messages;
  const {
    entries, loaded, streaming, reasoning, running, compacting, diagnostics, traceRuns, runQuestions,
    liveRunId, liveStartedAt, liveAgentName, liveAgentId, tasks, tasksLoaded, taskView, hasMore, loadingMore,
  } = state;
  const {
    onSend, onCancel, onApprove, onReject, onFork, onLoadEarlier, onSwitchBranch, onCompact, onRegenerate,
    onWatchTask, onUnwatchTask, onPatchTask, onLoadSpan, onPanelChange, onTerminalOpen,
  } = actions;
  const [agentConfigId, setAgentConfigIdState] = useState(() => loadSessionAgent(sessionId || ''));
  const [projectId, setProjectIdState] = useState(() => loadSessionProject(sessionId || ''));
  // The "New project…" dialog: pick a machine and a template, name the project.
  const [projDialogOpen, setProjDialogOpen] = useState(false);
  const [projSandboxId, setProjSandboxId] = useState('');
  const [projName, setProjName] = useState('');
  const [projEnv, setProjEnv] = useState<EnvVar[]>([]);
  const [projPorts, setProjPorts] = useState('');
  const [projSaving, setProjSaving] = useState(false);
  // The bound project whose environment is open for editing, and whether a
  // container call is in flight (both disable the menu).
  const [envProject, setEnvProject] = useState<Project | null>(null);
  const [containerBusy, setContainerBusy] = useState(false);
  // The bound project's compute state, refreshed when the menu's owner
  // changes and after every act on it. '' means "not asked yet".
  const [sandboxState, setSandboxState] = useState('');
  const [stateLoading, setStateLoading] = useState(false);

  useEffect(() => {
    setAgentConfigIdState(loadSessionAgent(sessionId || ''));
    setProjectIdState(loadSessionProject(sessionId || ''));
  }, [sessionId]);

  const setAgentConfigId = useCallback((id: string) => {
    setAgentConfigIdState(id);
    saveSessionAgent(sessionId || '', id);
  }, [sessionId]);
  // A user's explicit pick (not the auto-resolve below) is what a new chat
  // reopens on. Kept separate so merely opening old sessions doesn't move it.
  const pickAgent = useCallback((id: string) => {
    setAgentConfigId(id);
    saveLastAgent(id);
  }, [setAgentConfigId]);

  const setProjectId = useCallback((id: string) => {
    setProjectIdState(id);
    saveSessionProject(sessionId || '', id);
  }, [sessionId]);

  const [traceActiveRun, setTraceActiveRun] = useState<string | null>(null);
  // The span a Context panel jump asked the trace to open, and the counter that
  // makes repeating the same jump a fresh instruction.
  const [traceReveal, setTraceReveal] = useState<TraceReveal | null>(null);
  // A reveal is one instruction, not a standing state: once the trace panel
  // closes (or the session changes), a later manual open must not replay the
  // old jump's scroll.
  useEffect(() => {
    if (panel?.kind !== 'trace') setTraceReveal(null);
  }, [panel?.kind]);
  useEffect(() => {
    setTraceReveal(null);
  }, [sessionId]);
  const { data: agentConfigs, reload: reloadAgents } = useApi<AgentConfig[]>(() => api.agents.list() as Promise<AgentConfig[]>);
  const { data: sandboxDefs, reload: reloadSandboxes } = useApi<SandboxDef[]>(() => api.sandboxes.list() as Promise<SandboxDef[]>);
  // The caller's project rows for the picker — the same hook the terminal
  // panel's + menu uses.
  const { projects, error: projectsError, reload: reloadProjects, mutate: mutateProjects } = useProjects(bindingsVersion);

  useEffect(() => {
    if (!agentConfigs || agentConfigs.length === 0) return;
    if (agentConfigs.some(a => a.id === agentConfigId)) return; // a valid draft wins
    // No local draft for this session (a fork, another device, cleared storage).
    // Adopt the session's server-side agent once it has loaded — only then fall
    // back to the first agent — so the composer never silently runs a different
    // agent than the session is bound to.
    if (sessionAgentId === undefined) return; // still loading; don't guess yet
    if (sessionAgentId && agentConfigs.some(a => a.id === sessionAgentId)) {
      setAgentConfigId(sessionAgentId); // the session's server-side agent
      return;
    }
    // A new, unbound conversation: reopen on the last agent the user picked,
    // then fall back to the first in the list.
    const last = loadLastAgent();
    setAgentConfigId(agentConfigs.some(a => a.id === last) ? last : agentConfigs[0].id);
  }, [agentConfigs, agentConfigId, sessionAgentId, setAgentConfigId]);

  const confirmDialog = useConfirm();
  const deleteProject = async (p: Project) => {
    const ok = await confirmDialog({
      title: `Delete “${p.name}”?`,
      content: p.storage_hint
        ? `This DESTROYS its working tree — ${p.storage_hint} — and a Docker volume is not in anyone's backup. Export it first if it matters. Refused while sessions are still bound to it.`
        : 'This DESTROYS its working tree, and a Docker volume is not in anyone\'s backup. Export it first if it matters. Refused while sessions are still bound to it.',
      confirmButtonContent: 'Delete',
      confirmButtonType: 'danger',
    });
    if (!ok) return;
    try {
      const res = await api.projects.delete(p.id);
      if (res?.storage_error) {
        // The row IS gone; only the storage was left behind.
        toast.error(`“${p.name}” was deleted, but its storage could not be reclaimed: ${res.storage_error}`);
      }
      // Only a delete that actually happened drops the row and the selection:
      // a refused delete (still bound) must not flash the project out of the
      // list or clear the composer's pick.
      mutateProjects(prev => (prev ? prev.filter(x => x.id !== p.id) : prev));
      if (projectId === p.id) setProjectId('');
    } catch (e) {
      toast.error((e as Error).message || 'Could not delete the project');
    } finally {
      // Reconcile with the server whatever happened.
      reloadProjects();
    }
  };

  // A persisted project may have since been deleted — a send carrying it
  // would be refused, so a now-unknown id drops back to None.
  useEffect(() => {
    if (!projectId || !projects) return;
    if (!projects.some(p => p.id === projectId)) setProjectId('');
  }, [projects, projectId, setProjectId]);

  useEffect(() => {
    if (settingsReloadKey) { reloadAgents(); reloadSandboxes(); }
  }, [settingsReloadKey, reloadAgents, reloadSandboxes]);

  // The dep must change on every content growth, not just on new messages:
  // .chat-messages opts out of native scroll anchoring, so streamed text and
  // reasoning deltas only keep the view pinned if they re-fire this effect.
  const { ref: scrollRef, isSticky, scrollToBottom } = useScrollToBottom(
    messages.length + (streaming?.length ?? 0) + (reasoning?.length ?? 0),
    sessionId,
  );
  // Plain element handle alongside the hook's callback ref — the TOC rail
  // needs the scroll container for active-item tracking and jump targets.
  const chatElRef = useRef<HTMLDivElement | null>(null);
  const composedScrollRef = useCallback((node: HTMLDivElement | null) => {
    chatElRef.current = node;
    scrollRef(node);
  }, [scrollRef]);

  const handleCopyClick = useCallback((e: MouseEvent<HTMLDivElement>) => {
    const expand = (e.target as HTMLElement).closest('.btn-code-expand') as HTMLElement | null;
    if (expand) {
      expand.closest('.code-block-wrapper')?.classList.remove('code-collapsed');
      return;
    }
    const btn = (e.target as HTMLElement).closest('.btn-copy') as HTMLElement | null;
    if (!btn) return;
    // getAttribute already returns the decoded value (the HTML parser resolved
    // the entities the renderer escaped into the attribute). A second manual
    // decode here corrupted any code that literally contained an entity like
    // "&amp;", so read the attribute as-is.
    const code = btn.getAttribute('data-code');
    if (!code) return;
    navigator.clipboard.writeText(code).then(() => {
      btn.classList.add('copied');
      const svgContent = btn.innerHTML;
      btn.innerHTML = CHECK_ICON;
      setTimeout(() => { btn.classList.remove('copied'); btn.innerHTML = svgContent; }, 1500);
    });
  }, []);

  const selectedProject = projects?.find(p => p.id === projectId);
  const sandboxName = (id?: string) => sandboxDefs?.find(sb => sb.id === id)?.name || '';
  // The bound pair is what the top-bar menu acts on: a session's container is
  // its binding's, never the composer's current pick.
  const boundProject = sessionBinding?.projectId ? projects?.find(p => p.id === sessionBinding.projectId) || null : null;
  const boundSandbox = sandboxDefs?.find(sb => sb.id === boundProject?.sandbox_id);
  // Whether the bound project's sandbox row is known: false while sandboxDefs
  // load, and for an orphan project whose row was deleted. The menu leans on
  // this so a danger action (Rebuild) is never offered on a guess.
  const boundSandboxKnown = !!boundProject && !!boundSandbox;

  // The state is read when the bound project changes, again as the menu opens,
  // and whenever a run starts or ends: a run's first command starts the
  // sandbox without telling this component, so a state read once at bind time
  // goes stale in the most ordinary way there is. A failure leaves the last
  // known value in place (or, on a first read, offers Start — the harmless
  // choice). A stale response never wins: only the newest read for the current
  // project lands.
  const stateReqSeq = useRef(0);
  const refreshSandboxState = useCallback(async (projectID: string) => {
    const seq = ++stateReqSeq.current;
    setStateLoading(true);
    try {
      const state = (await api.projects.sandboxStatus(projectID)).state;
      if (seq === stateReqSeq.current) setSandboxState(state);
    } catch {
      // Keep the last known value; '' stays '' and renders Start, not a
      // permanent "Checking…".
    } finally {
      if (seq === stateReqSeq.current) setStateLoading(false);
    }
  }, []);
  useEffect(() => {
    if (!boundProject) { stateReqSeq.current++; setSandboxState(''); return; }
    void refreshSandboxState(boundProject.id);
  }, [boundProject, refreshSandboxState]);
  // A run's first command starts the container and its end lets the idle timer
  // stop it; either edge can move the state without a message to this
  // component, so re-read on both.
  useEffect(() => {
    if (boundProject) void refreshSandboxState(boundProject.id);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [running]);

  // Start and stop are synchronous and can take an image pull's worth of
  // time, so the menu stays disabled until they answer.
  const startSandbox = async () => {
    if (!boundProject || containerBusy) return;
    setContainerBusy(true);
    toast.info('Starting the sandbox…');
    try {
      await api.projects.sandboxStart(boundProject.id);
      setSandboxState('running');
      toast.success('Sandbox running');
    } catch (e) {
      toast.error((e as Error).message || 'Could not start the sandbox');
    } finally {
      setContainerBusy(false);
    }
  };

  const stopSandbox = async () => {
    if (!boundProject || containerBusy) return;
    setContainerBusy(true);
    try {
      const res = await api.projects.sandboxStop(boundProject.id);
      if (res.stopped) {
        setSandboxState('stopped');
        toast.success('Sandbox stopped — the files are kept');
      } else {
        // A run or an open terminal is still using it; it stops when that ends.
        toast.info('Will stop when the work using it finishes');
      }
    } catch (e) {
      toast.error((e as Error).message || 'Could not stop the sandbox');
    } finally {
      setContainerBusy(false);
    }
  };

  // The port is asked for rather than guessed: a project may run several
  // services, and the one a person wants is the one they type. Where Preview
  // reaches any port (`any_port`), asking is the only way in: there is no
  // declared list to choose from.
  const askPreviewPort = async () => {
    // A backend that serves previewed ports on a PUBLIC host must say so
    // before opening: the grant is only a convenience, not a guard.
    const msg = boundSandbox?.supports?.public_ports
      ? 'Which port inside the sandbox?\n\nHeads up: this opens a public URL — anyone with the link can reach the port.'
      : 'Which port inside the sandbox?';
    const raw = window.prompt(msg, '3000');
    if (!raw) return;
    const port = parseInt(raw, 10);
    if (!Number.isFinite(port) || port <= 0 || port > 65535) {
      toast.error('That is not a port');
      return;
    }
    await previewPort(port);
  };

  // Preview and export read the container; they do not change its lifecycle, so
  // they never take the containerBusy lock that Start/Stop/Rebuild hold — a
  // slow export must not lock the menu shut on the state itself. Their own refs
  // only stop a double-click.
  const previewBusy = useRef(false);
  const exportBusy = useRef(false);

  const previewPort = async (port: number) => {
    if (!boundProject || previewBusy.current) return;
    previewBusy.current = true;
    // Open the tab inside the click, before the await: a window.open after the
    // grant resolves is outside the gesture and pop-up blockers eat it. The
    // opener is severed at once so the untrusted preview (a separate origin)
    // cannot reach back through window.opener.
    const win = window.open('about:blank', '_blank');
    if (win) win.opener = null;
    try {
      const grant = await api.projects.previewGrant(boundProject.id, port);
      if (win) win.location.href = grant.url;
      else toast.error('A pop-up blocker stopped the preview tab');
    } catch (e) {
      win?.close();
      toast.error((e as Error).message || 'Could not open a preview');
    } finally {
      previewBusy.current = false;
    }
  };

  const exportProject = async () => {
    if (!boundProject || exportBusy.current) return;
    exportBusy.current = true;
    toast.info('Preparing the archive…');
    try {
      await api.projects.exportTar(boundProject.id, boundProject.name);
    } catch (e) {
      toast.error((e as Error).message || 'Could not export the project');
    } finally {
      exportBusy.current = false;
    }
  };

  // A rebuild is synchronous and can take an image pull's worth of time, so
  // the menu stays disabled until it answers.
  const rebuildContainer = async () => {
    if (!boundProject || containerBusy) return;
    if (!await confirmDialog({
      title: `Rebuild the container for “${boundProject.name}”?`,
      content: 'The container is discarded and created again from the image. Files under /workspace survive; anything installed into the container does not, and commands running in it right now will fail.',
      confirmButtonType: 'danger',
    })) return;
    setContainerBusy(true);
    toast.info('Rebuilding the container…');
    try {
      await api.projects.rebuildContainer(boundProject.id);
      setSandboxState('running');
      toast.success('Container rebuilt');
    } catch (e) {
      toast.error((e as Error).message || 'The rebuild failed');
    } finally {
      setContainerBusy(false);
    }
  };
  const sandboxView = composerSandboxView(sessionBinding || null, projects, sandboxDefs);

  const handleSend = useCallback((text: string) => {
    // No sessionId is fine: sending with no active session starts a new session
    // (app-level onSend auto-creates it). Only an agent is required.
    if (!agentConfigId) return;
    // Bound: the server uses the binding regardless — send no project claim.
    onSend(text, agentConfigId, sessionBinding ? '' : projectId);
  }, [agentConfigId, projectId, sessionBinding, onSend]);


  const handleCancel = useCallback((graceful?: boolean) => {
    if (!onCancel(graceful)) {
      toast.error('Could not send the stop — not connected, or no run to stop');
      return;
    }
    toast.info(graceful ? 'Stopping after the current turn…' : 'Run cancelled');
  }, [onCancel]);

  // A wake run's input is the raw notification text — label it by what it
  // delivers, phrased so it reads as the parent's reaction to the result, not
  // as the task's own trace (the task's trace lives in the Inspector). A
  // workflow execution is a task too, so its notification parses the same
  // way; the kind, from the task rows, picks the word.
  const runLabelFor = useCallback((content: string) => {
    const notif = parseTaskNotification(content);
    if (!notif) return content;
    const labels = notif.items.map(it => it.label).filter(Boolean);
    const which = labels.length > 1 ? labels.join(', ') : (labels[0] || notif.taskId || '');
    if (!which) return notif.text.split('\n')[0];
    const workflow = notif.items.length > 0 && notif.items.every(it => it.taskId && tasks?.[it.taskId]?.kind === TASK_KIND_WORKFLOW);
    return (workflow ? 'workflow result: ' : 'task result: ') + which;
  }, [tasks]);

  const { turnRunMap, userRunMap, runLabels, staleRuns } = useMemo(() => {
    const tMap: Record<number, string> = {};
    const uMap: Record<number, string> = {};
    const labels: Record<string, string> = {};
    // A workflow-started note is the question of an exchange no run asked:
    // the wake-up run that later delivers that execution's result is labeled
    // by it and jumps to it. Notes precede their results in the timeline.
    const noteIdxByTask: Record<string, number> = {};
    let turnIdx = 0;
    for (let i = 0; i < messages.length; i++) {
      const entry = messages[i] as unknown as { role: string; runId?: string; content?: string; note?: WorkflowStartedNote };
      const rid = entry.runId;
      if (entry.role === 'system' && entry.note?.taskId) {
        noteIdxByTask[entry.note.taskId] = i;
        continue;
      }
      // Label runs from the user message directly, so a run whose reply
      // produced no visible turn still shows its question in the trace panel.
      if (entry.role === 'user' && rid && traceRuns[rid]) {
        const notif = parseTaskNotification(entry.content);
        // Notifications don't render, so they anchor no jump target — label
        // the run but keep it out of userRunMap/messageRunIds — unless the
        // execution's start left a note, which then IS the anchor.
        if (!notif) uMap[i] = rid;
        if (entry.content && !labels[rid]) labels[rid] = runLabelFor(entry.content);
        if (notif) {
          const noted = notif.items.find(it => it.taskId && noteIdxByTask[it.taskId] !== undefined);
          if (noted?.taskId !== undefined && noted.taskId !== null) {
            const idx = noteIdxByTask[noted.taskId];
            const note = (messages[idx] as unknown as { note: WorkflowStartedNote }).note;
            uMap[idx] = rid;
            labels[rid] = '▶ ' + (note.workflowName || noted.label) + ' (' + originText(note.origin) + ')';
          }
        }
      } else if (entry.role === 'turn') {
        if (rid && traceRuns[rid]) {
          tMap[i] = rid;
          let userContent: string | null = null;
          for (let j = i - 1; j >= 0; j--) {
            if (messages[j].role === 'user') {
              userContent = messages[j].content ?? null;
              // The turn's run OVERWRITES the one the user message carries.
              // A message's own run_id is whichever run first produced it —
              // after a regenerate that is an attempt the session has since
              // branched away from, and it would claim the jump target for a
              // run no longer in the timeline while the current attempt got
              // none. On the active branch a message is followed by exactly
              // one turn, so there is nothing to contend over.
              if (!parseTaskNotification(messages[j].content)) uMap[j] = rid;
              break;
            }
          }
          if (!labels[rid]) {
            labels[rid] = userContent ? runLabelFor(userContent) : 'Turn ' + (turnIdx + 1);
          }
        }
        turnIdx++;
      }
    }
    // Runs whose turn is NOT in the rendered timeline: a regenerated answer
    // the session has since branched away from. Their traces are still listed
    // — the work happened — but the timeline has no turn to label them from,
    // so they fell back to a raw run id. Label them from the entries instead,
    // and mark them, so "5 traces, 3 exchanges" reads as what it is rather
    // than as a mismatch.
    const stale = new Set<string>();
    const paged = new Set<string>();
    let lastUser: string | null = null;
    for (const e of entries || []) {
      if (e.role === 'user' && e.content) lastUser = e.content;
      const rid = e.run_id;
      if (!rid || !traceRuns[rid]) continue;
      paged.add(rid);
      if (e.on_path === false) stale.add(rid);
      if (!labels[rid] && lastUser) labels[rid] = runLabelFor(lastUser);
    }
    // Runs whose exchange lies before the page of history loaded: the timeline
    // pages, the traces do not. The server's own walk over ALL the entries
    // (GET /sessions/:id/runs) names them the same way, and says which of them
    // the session has branched away from. A run the page holds is the page's
    // to judge — its entries are current, this snapshot is from the trace load.
    for (const [rid, q] of Object.entries(runQuestions || {})) {
      if (!traceRuns[rid] || paged.has(rid)) continue;
      if (!labels[rid] && q.question) labels[rid] = runLabelFor(q.question);
      if (!q.onPath) stale.add(rid);
    }
    return { turnRunMap: tMap, userRunMap: uMap, runLabels: labels, staleRuns: stale };
  }, [messages, entries, traceRuns, runQuestions, runLabelFor]);

  // Wake-up run → the run whose spawn_task started the chain, read straight
  // off the trace: a wake run's spans carry parent_run_id, recorded at launch.
  // The lineage lives on the run's own durable output — deriving it here from
  // task rows and notification text broke on every surface that does not carry
  // them (a fork copies traces but not task rows; a fold moves the
  // notification out of the rendered timeline).
  const traceRunParents = useMemo(() => {
    const parents: Record<string, string> = {};
    for (const [rid, evs] of Object.entries(traceRuns)) {
      for (const ev of evs) {
        if (ev.parent_run_id && ev.parent_run_id !== rid) {
          parents[rid] = ev.parent_run_id;
          break;
        }
      }
    }
    return parents;
  }, [traceRuns]);

  const openTrace = useCallback((runId: string) => {
    onPanelChange({ kind: 'trace' });
    setTraceActiveRun(runId);
  }, [onPanelChange]);

  const inspectTask = useCallback((taskId: string) => {
    onPanelChange({ kind: 'task', taskId });
  }, [onPanelChange]);

  // The task context: the socket's task state, which the durable rows seed and
  // task.updated keeps current, as the list the strip / Tasks panel / top bar
  // read plus the per-call lookups the tool cards read.
  const chatTasks = useDerivedChatTasks(tasks);
  const backgroundItems = chatTasks.items;
  const inspectedItem = panel?.kind === 'task' ? backgroundItems.find(it => it.id === panel.taskId) : undefined;

  const stopTask = useCallback(async (taskId: string) => {
    try {
      const info = await (api.tasks.stop(taskId) as Promise<{ status?: string }>);
      // Apply the confirmed state directly: after a restart the hub has no
      // record of the run, so no run.cancelled broadcast will arrive.
      if (sessionId && info?.status) {
        onPatchTask?.(sessionId, taskId, { status: info.status as TaskStatus, pendingCallId: undefined, pendingToolName: undefined });
      }
    } catch (e) {
      toast.error((e as Error).message || 'Stop failed');
    }
  }, [sessionId, onPatchTask]);

  const retryTask = useCallback(async (taskId: string) => {
    try {
      const info = await (api.tasks.retry(taskId) as Promise<{ status?: string; attempt?: number; max_attempts?: number }>);
      // The confirmed state, applied without waiting for the broadcast — the
      // same reason stopTask does: the answer is already in hand, and a button
      // that stays on "failed" invites a second click that will be refused. The
      // failed attempt's summary goes with it.
      if (sessionId && info?.status) {
        onPatchTask?.(sessionId, taskId, {
          status: info.status as TaskStatus, attempt: info.attempt,
          maxAttempts: info.max_attempts, summary: undefined,
        });
      }
    } catch (e) {
      toast.error((e as Error).message || 'Retry failed');
    }
  }, [sessionId, onPatchTask]);

  // Hides a finished bar without giving up the row (the Tasks panel keeps it,
  // and a retry un-dismisses server-side). The server announces the
  // dismissal (task.updated) to every window; the local patch only spares
  // this one the round trip.
  const dismissTask = useCallback(async (taskId: string) => {
    try {
      await api.tasks.dismiss(taskId);
      if (sessionId) onPatchTask?.(sessionId, taskId, { dismissed: true });
    } catch (e) {
      toast.error((e as Error).message || 'Dismiss failed');
    }
  }, [sessionId, onPatchTask]);

  // The detail lens is live: tell the socket layer which child session to tail.
  const inspectedChild = inspectedItem?.childSessionId;
  const inspectedId = inspectedItem?.id;
  useEffect(() => {
    if (!sessionId || !inspectedId || !inspectedChild || !onWatchTask || !onUnwatchTask) return;
    onWatchTask(sessionId, inspectedId, inspectedChild);
    return () => onUnwatchTask(sessionId);
  }, [sessionId, inspectedId, inspectedChild, onWatchTask, onUnwatchTask]);

  // Runs that have a user message in this conversation — gates the trace
  // panel's jump-to-message control.
  // Runs the trace panel can offer a "jump to message" for: those with
  // something in the RENDERED timeline to jump to. A run whose attempt was
  // regenerated away has no anchor — the jump would scroll to nothing — so it
  // gets no button, which is also what distinguishes it from the attempt that
  // replaced it.
  const messageRunIds = useMemo(
    () => new Set([...Object.values(userRunMap), ...Object.values(turnRunMap)]),
    [userRunMap, turnRunMap],
  );

  // Reverse navigation: scroll the chat to the run's user message and flash
  // it, mirroring the message → trace direction of openTrace.
  const jumpToRun = useCallback((runId: string) => {
    const el = document.querySelector(`.chat-messages [data-run-id="${CSS.escape(runId)}"]`);
    if (!el) return;
    el.scrollIntoView({ behavior: 'smooth', block: 'center' });
    flashMessage(el);
  }, []);

  // TOC rail: one entry per user prompt; click scrolls to the message. The
  // upward smooth scroll trips the scroll hook's moved-up intent detection,
  // so following pauses automatically while the user reads.
  const tocItems = useMemo(() =>
    messages.flatMap((m, i) => m.role === 'user' && m.content && !parseTaskNotification(m.content)
      ? [{ idx: i, preview: m.content.replace(/\s+/g, ' ').trim().slice(0, 60) }]
      : []),
    [messages]);

  const jumpToMsg = useCallback((idx: number) => {
    const el = chatElRef.current?.querySelector(`[data-msg-idx="${idx}"]`);
    if (!el) return;
    el.scrollIntoView({ behavior: 'smooth', block: 'start' });
    flashMessage(el);
  }, []);

  // Bound sessions claim no project (the server uses the binding); unbound
  // ones carry the choice, because a regen can be the first project-carrying
  // run.
  const regenProjectId = sessionBinding ? '' : projectId;
  const handleRegen = useCallback((messageId: string, content: string) => {
    onRegenerate?.(messageId, content, agentConfigId, regenProjectId);
  }, [onRegenerate, agentConfigId, regenProjectId]);

  // The session scope every transcript component reads (see
  // ChatSessionContext for the split). Each value is memoized on its inputs so
  // a streaming delta — which changes none of them — re-renders no consumer.
  const agentAvatars = useMemo<Record<string, string>>(() => {
    const m: Record<string, string> = {};
    for (const a of agentConfigs || []) if (a.avatar) m[a.id] = a.avatar;
    return m;
  }, [agentConfigs]);
  const session = useMemo<ChatSessionState>(
    () => ({
      sessionId, running, compacting, liveAgentName, liveStartedAt, diagnostics, agentAvatars,
      liveAgentAvatar: (liveAgentId && agentAvatars[liveAgentId]) || null,
    }),
    [sessionId, running, compacting, liveAgentName, liveAgentId, agentAvatars, liveStartedAt, diagnostics],
  );
  const turnActions = useMemo<ChatActions>(() => ({
    approve: onApprove, reject: onReject, fork: onFork, switchBranch: onSwitchBranch,
    regenerate: onRegenerate ? handleRegen : undefined,
    openTrace, inspectTask, retryTask, stopTask, dismissTask, loadSpan: onLoadSpan,
  }), [onApprove, onReject, onFork, onSwitchBranch, onRegenerate, handleRegen, openTrace, inspectTask, retryTask, stopTask, dismissTask, onLoadSpan]);

  const topBar = (
    <ChatTopBar
      sessionName={sessionName || ''}
      panel={panel}
      onPanelChange={onPanelChange}
      terminalEnabled={!!onTerminalOpen && !!sandboxDefs && sandboxDefs.length > 0}
      onTerminalOpen={onTerminalOpen
        ? () => {
          // A bound session's terminal follows its binding — the same
          // container the runs use. Unbound sessions fall back to the
          // picker's current selection; a terminal cannot open without a
          // project.
          const pid = sessionBinding?.projectId || projectId;
          const project = pid ? projects?.find(p => p.id === pid) : undefined;
          onTerminalOpen(pid
            ? { projectId: pid, projectName: project?.name || '', targetName: sandboxName(project?.sandbox_id) }
            : undefined);
        }
        : undefined}
      binding={sandboxView.bound && sessionBinding
        ? { title: sandboxView.title, projectName: projects?.find(p => p.id === sessionBinding.projectId)?.name || '…' }
        : null}
      projectMenu={boundProject ? {
        busy: containerBusy,
        state: sandboxState,
        stateLoading,
        // Capabilities come from the sandbox row the project names. Until that
        // row declares `rebuild`, Rebuild is withheld rather than offered on a
        // guess: a backend whose store IS the compute cannot be rebuilt, and a
        // mislabelled Rebuild there would read as "safe" when it is refused.
        rebuildable: boundSandboxKnown && !!boundSandbox?.supports?.rebuild,
        anyPort: !!boundSandbox?.supports?.any_port,
        onEnv: () => setEnvProject(boundProject),
        onStart: () => { void startSandbox(); },
        onStop: () => { void stopSandbox(); },
        onExport: () => { void exportProject(); },
        ports: boundProject.ports || [],
        onPreview: (port: number) => { void previewPort(port); },
        onPreviewAsk: () => { void askPreviewPort(); },
        onRebuild: () => { void rebuildContainer(); },
        onOpen: () => { void refreshSandboxState(boundProject.id); },
      } : null}
    />
  );

  // Every branch below renders inside the session scope — the top bar reads it
  // even with no session open. No hooks past this point.
  const scoped = (body: ReactNode) => (
    <ChatSessionProvider session={session} actions={turnActions} tasks={chatTasks}>{body}</ChatSessionProvider>
  );

  if (!sessionId) {
    return scoped(
      <div className="chat-main">
        <div className="chat-content">
          {topBar}
          <div className="chat-empty">
            <Blankslate>
              <Blankslate.Visual>
                <CommentDiscussionIcon size={24} />
              </Blankslate.Visual>
              <Blankslate.Heading>Start a conversation</Blankslate.Heading>
              <Blankslate.Description>Pick a chat from the sidebar, or create a new one to begin.</Blankslate.Description>
            </Blankslate>
          </div>
        </div>
      </div>
    );
  }

  const selectedAgent = agentConfigs?.find(a => a.id === agentConfigId);
  const selectedAgentLabel = selectedAgent?.name || 'Agent';

  const inputToolbar: ReactNode = (
    <>
      <div className="chat-input-toolbar-left">
        {/* Bound sessions show nothing here — the binding lives in the top
            bar's badge. Before binding the picker offers PROJECTS — the
            caller's rows, grouped by sandbox — because the project is what a
            person recognizes; the backend is its attribute, not the other
            way around. */}
        {!sandboxView.bound && sandboxDefs && sandboxDefs.length > 0 && (
          <ActionMenu>
            {/* Nothing picked yet reads as an offer, "+", not as a folder
                that is not there; a picked project shows as itself. */}
            {selectedProject ? (
              <ActionMenu.Button size="small" variant="invisible" leadingVisual={FileDirectoryIcon}>
                {projectLabel(selectedProject.name, sandboxName(selectedProject.sandbox_id))}
              </ActionMenu.Button>
            ) : (
              <ActionMenu.Anchor>
                <IconButton icon={PlusIcon} size="small" variant="invisible" aria-label="Project" />
              </ActionMenu.Anchor>
            )}
            <ActionMenu.Overlay>
              <ActionList selectionVariant="single">
                <ActionList.Item selected={projectId === ''} onSelect={() => setProjectId('')}>
                  None
                  <ActionList.Description variant="inline">chat only</ActionList.Description>
                </ActionList.Item>
                {/* A failed fetch must not read as an empty account. */}
                {projectsError && <ActionList.Item disabled>projects failed to load</ActionList.Item>}
                {/* One group per machine: the group heading carries the
                    target, rows carry just the project name. */}
                {groupProjects(projects, sandboxDefs).map(g => (
                  <ActionList.Group key={g.sandboxId}>
                    {/* Inside a menu-role ActionList the heading is
                        presentational: a heading level (`as`) is invalid in a
                        menu and throws, unmounting the app. */}
                    <ActionList.GroupHeading>{g.sandboxName}</ActionList.GroupHeading>
                    {g.items.map(p => (
                      <ActionList.Item
                        key={p.id}
                        selected={projectId === p.id}
                        onSelect={() => setProjectId(p.id)}
                        title={projectLabel(p.name, g.sandboxName)}
                      >
                        {p.name}
                      </ActionList.Item>
                    ))}
                  </ActionList.Group>
                ))}
                <ActionList.Divider />
                <ActionList.Item
                  onSelect={() => {
                    // Default to the composer's current project's sandbox, else
                    // one that does not serve ports publicly: a first-in-list
                    // default that lands on a paid, publicly-reachable cloud
                    // sandbox is a footgun.
                    const fallback = sandboxDefs.find(sb => !sb.supports?.public_ports) || sandboxDefs[0];
                    setProjSandboxId(selectedProject?.sandbox_id || fallback.id);
                    setProjName('');
                    setProjEnv([]);
                    setProjPorts('');
                    setProjDialogOpen(true);
                  }}
                >
                  New project…
                </ActionList.Item>
                {/* A plain menu item, not a per-row trailing action: Primer
                    forbids ActionList.TrailingAction inside a menu-role list
                    (it would not render). Deleting acts on the SELECTED
                    project, behind its own confirm. */}
                {selectedProject && (
                  <ActionList.Item variant="danger" onSelect={() => void deleteProject(selectedProject)}>
                    Delete “{selectedProject.name}”…
                  </ActionList.Item>
                )}
              </ActionList>
            </ActionMenu.Overlay>
          </ActionMenu>
        )}
        {projDialogOpen && (() => {
          const projSandbox = sandboxDefs?.find(sb => sb.id === projSandboxId);
          const create = async () => {
            if (!projSandbox || !projName.trim() || projSaving) return;
            setProjSaving(true);
            try {
              const created = await api.projects.create({
                name: projName.trim(),
                sandbox_id: projSandbox.id,
                env: cleanEnv(projEnv),
                ports: parsePorts(projPorts) ?? [],
              }) as Project;
              // Seed the cached list before selecting: the stale-id guard
              // below runs against `projects` on the very next commit, and a
              // fire-and-forget reload would hand it a list without the new
              // row — wiping the selection it should protect.
              mutateProjects(prev => prev ? [...prev.filter(p => p.id !== created.id), created] : [created]);
              if (created.id) setProjectId(created.id);
              reloadProjects();
              setProjDialogOpen(false);
            } catch (e) {
              // 409: a project of that name already exists on this machine.
              toast.error((e as Error).message || 'Could not create the project');
            } finally {
              setProjSaving(false);
            }
          };
          return (
            <Dialog
              title="New project"
              onClose={() => setProjDialogOpen(false)}
              width="large"
              footerButtons={[
                { content: 'Cancel', onClick: () => setProjDialogOpen(false) },
                {
                  content: projSaving ? 'Creating…' : 'Create',
                  buttonType: 'primary',
                  disabled: !projSandbox || !projName.trim() || projSaving || !!envError(projEnv) || parsePorts(projPorts) === null,
                  onClick: () => { void create(); },
                },
              ]}
            >
              <Stack gap="normal">
                {fc('Sandbox', (
                  <Select block value={projSandboxId} onChange={e => setProjSandboxId(e.target.value)}>
                    {sandboxDefs?.map(sb => (
                      <Select.Option key={sb.id} value={sb.id}>{sb.name}</Select.Option>
                    ))}
                  </Select>
                ), 'The machine the files live on and the image they run in. The machine is fixed once the project exists; the image can change.')}
                {fc('Name', (
                  <TextInput
                    block
                    value={projName}
                    placeholder="e.g. goagents"
                    onChange={e => setProjName(e.target.value)}
                  />
                ), 'Names the working tree the container mounts; unique per sandbox.')}
                {/* The editor carries its own explanation; a caption here
                    would be a third line of small print saying the same. */}
                {fc('Environment', (
                  <EnvEditor vars={projEnv} onChange={setProjEnv} disabled={projSaving} />
                ), envError(projEnv))}
                {/* Hidden where Preview already reaches any port (`any_port`):
                    there is nothing to declare. */}
                {!projSandbox?.supports?.any_port && fc('Published ports', (
                  <TextInput
                    block
                    value={projPorts}
                    placeholder="3000, 5173"
                    onChange={e => setProjPorts(e.target.value)}
                  />
                ), parsePorts(projPorts) === null
                  ? 'Ports must be numbers between 1 and 65535, separated by commas'
                  : 'What Preview can open. A server inside must listen on 0.0.0.0. Changeable later.')}
              </Stack>
            </Dialog>
          );
        })()}
        {envProject && (
          <ProjectEnvDialog
            project={envProject}
            sessionCount={envProject.session_count}
            onClose={() => { setEnvProject(null); reloadProjects(); }}
          />
        )}
      </div>
      <div className="chat-input-toolbar-right">
        {agentConfigs && agentConfigs.length > 0 ? (
          <ActionMenu>
            {/* Text-only: at composer size an avatar reads as clutter; the
                dropdown rows carry the avatars. */}
            <ActionMenu.Button size="small" variant="invisible">
              {selectedAgentLabel}
            </ActionMenu.Button>
            <ActionMenu.Overlay>
              <ActionList selectionVariant="single">
                {agentConfigs.map(a => (
                  <ActionList.Item key={a.id} selected={agentConfigId === a.id} onSelect={() => pickAgent(a.id)}>
                    <ActionList.LeadingVisual>
                      <AgentAvatar name={a.name} avatar={a.avatar} size={20} />
                    </ActionList.LeadingVisual>
                    {a.name}
                  </ActionList.Item>
                ))}
              </ActionList>
            </ActionMenu.Overlay>
          </ActionMenu>
        ) : (
          <span className="chat-input-toolbar-warn">No agents — go to Settings</span>
        )}
      </div>
    </>
  );

  // One transcript row. Extracted from the render so the workflow grouping can
  // wrap a span of them without the map body moving.
  const renderMessage = (m: ChatMessage, i: number) => {
    if (m.role === 'turn') {
        const isLive = running && i === messages.length - 1;
        // The user message this turn answers — the row object itself, whose
        // identity is stable across deltas, so the memoized block stays put.
        let prompt: ChatMessage | null = null;
        for (let j = i - 1; j >= 0; j--) {
          if (messages[j].role === 'user') { prompt = messages[j]; break; }
        }
        const rid = turnRunMap[i];
        const turnDuration = rid && traceRuns[rid]
          ? traceRuns[rid].find(e => e.kind === 'span' && e.duration)?.duration
          : undefined;
        return (
          <TurnBlock
            key={entryKey(m, i, 'turn')}
            parts={m.parts || []}
            streaming={isLive ? streaming : null}
            reasoning={isLive ? reasoning : null}
            isLive={isLive}
            prompt={prompt}
            duration={turnDuration}
            messageId={m.messageId}
            branches={m.branches}
          />
        );
      }
      if (m.role === 'user') {
        const rid = userRunMap[i];
        return (
          <UserMessage
            key={entryKey(m, i, 'user')}
            content={m.content || ''}
            traceRunId={rid || null}
            msgIdx={i}
            entryId={m.entryId}
          />
        );
      }
      if (m.role === 'compaction') {
        return <CompactionCard key={entryKey(m, i, 'compaction')} {...m} />;
      }
      if (m.role === 'system' && m.note) {
        return <WorkflowStartedChip key={entryKey(m, i, 'msg')} note={m.note} content={m.content || ''} traceRunId={userRunMap[i] || null} msgIdx={i} entryId={m.entryId} />;
      }
      return <MessageBubble key={entryKey(m, i, 'msg')} role={m.role} content={m.content || ''} />;
  };

  const isEmpty = loaded && messages.length === 0;

  // The Inspector's lenses. Rendered by the empty branch too: a session that
  // has never spoken can still own background work — a workflow started on
  // it — and its strip opens the task lens.
  const sidePanels = (
    <>
      {panel?.kind === 'trace' && (
        <TraceDrawer
          traceRuns={traceRuns}
          liveRunId={liveRunId}
          activeRunId={traceActiveRun}
          runLabels={runLabels}
          staleRuns={staleRuns}
          runParents={traceRunParents}
          onClose={() => onPanelChange(null)}
          onJumpToRun={jumpToRun}
          messageRunIds={messageRunIds}
          reveal={traceReveal || undefined}
        />
      )}
      {panel?.kind === 'context' && sessionId && (
        <ContextPanel
          sessionId={sessionId}
          running={running}
          // entries is replaced by every server re-read of the timeline — a
          // branch switch included, which running alone would miss.
          reloadKey={entries}
          onClose={() => onPanelChange(null)}
          onCompact={onCompact}
        />
      )}
      {panel?.kind === 'tasks' && (
        <BackgroundListPanel onClose={() => onPanelChange(null)} />
      )}
      {panel?.kind === 'task' && inspectedItem && (
        <BackgroundDetailPanel
          item={inspectedItem}
          view={taskView && taskView.taskId === inspectedItem.id ? taskView : null}
          onBack={() => onPanelChange({ kind: 'tasks' })}
          onClose={() => onPanelChange(null)}
        />
      )}
      {panel?.kind === 'task' && !inspectedItem && (
        <BackgroundMissingPanel
          taskId={panel.taskId}
          loading={!tasksLoaded}
          onBack={() => onPanelChange({ kind: 'tasks' })}
          onClose={() => onPanelChange(null)}
        />
      )}
    </>
  );


  if (!loaded && messages.length === 0) {
    return scoped(
      <div className="chat-main">
        <div className="chat-content">
          {topBar}
        </div>
      </div>
    );
  }

  if (isEmpty) {
    return scoped(
      <div className={'chat-main' + (panel ? ' trace-open' : '')}>
        <div className="chat-content">
          {topBar}
          <div className="chat-content chat-content-centered">
            <Greeting key={`greeting-${sessionId}`} />
            <WorkflowStrip />
            <MessageInput
              key={`input-${sessionId}`}
              sessionId={sessionId}
              onSend={handleSend}
              onCancel={handleCancel}
              disabled={running || awaiting || !agentConfigId}
              running={running}
              toolbar={inputToolbar}
            />
          </div>
        </div>
        {sidePanels}
      </div>
    );
  }

  return scoped(
    <div className={'chat-main' + (panel ? ' trace-open' : '')}>
      <div className="chat-content">
        {topBar}
        <div className="chat-messages-area">
        <div ref={composedScrollRef} className="chat-messages" onClick={handleCopyClick}>
          {hasMore && (
            <div className="load-earlier">
              <Button size="small" variant="invisible" onClick={onLoadEarlier} disabled={loadingMore}>
                {loadingMore ? 'Loading…' : 'Load earlier messages'}
              </Button>
            </div>
          )}
          {messages.map(renderMessage)}
        </div>

        {!isSticky && (
          <Button size="small" leadingVisual={ArrowDownIcon} className="scroll-to-bottom" onClick={scrollToBottom}>
            {streaming ? 'Responding…' : 'Jump to latest'}
          </Button>
        )}
        <ChatToc items={tocItems} scrollElRef={chatElRef} onJump={jumpToMsg} />
        </div>
        <WorkflowStrip />
        <MessageInput
          key={`input-${sessionId}`}
          sessionId={sessionId}
          onSend={handleSend}
          onCancel={handleCancel}
          disabled={running || awaiting || !agentConfigId}
          running={running}
          toolbar={inputToolbar}
        />
      </div>

      {sidePanels}
    </div>
  );
}
