import './chat.css';
import { useState, useEffect, useCallback, useMemo, useRef, type MouseEvent, type ReactNode } from 'react';
import { Button, ActionMenu, ActionList, Link } from '@primer/react';
import { api } from '@/lib/api';
import { CHECK_ICON } from '@/lib/markdownShared';
import { type TurnPart, type TimelineEntry, type Branches, type WorkflowStartedNote } from '@/lib/timeline';
import { useScrollToBottom, useApi, useCopy } from '@/lib/hooks';
import { loadSessionAgent, saveSessionAgent, loadLastAgent, saveLastAgent, loadSessionProject, saveSessionProject } from '@/lib/drafts';
import { composerProjectRows, composerSandboxView, projectLabel, type SandboxSupports, type SessionBinding } from '@/lib/binding';
import { useProjects } from '@/lib/useProjects';
import { parseTaskNotification, TASK_KIND_WORKFLOW, type TaskStatus } from '@/lib/protocol';
import type { SessionState, TaskState } from '@/lib/useAgentSocket';
import { ChatSessionProvider, useDerivedChatTasks, type ChatSessionState, type ChatActions } from '@/features/chat/ChatSessionContext';
import { BackgroundListPanel, BackgroundDetailPanel, BackgroundMissingPanel } from '@/features/chat/BackgroundPanel';
import { AgentAvatar } from '@/components/AgentAvatar';
import { ScopeHint, collidingNames } from '@/components/AgentPicker';
import { Loading } from '@/components/Loading';
import { MessageBubble } from '@/features/chat/MessageBubble';
import { TurnBlock } from '@/features/chat/TurnBlock';
import { UserMessage } from '@/features/chat/UserMessage';
import { WorkflowStartedChip, originText } from '@/features/chat/WorkflowStartedChip';
import { CompactionCard } from '@/features/chat/CompactionCard';
import { Greeting } from '@/features/chat/Greeting';
import { ChatToc } from '@/features/chat/ChatToc';
import { MessageInput } from '@/features/chat/MessageInput';
import type { AttachmentMeta } from '@/lib/attachments';
import { WorkflowStrip } from '@/features/chat/WorkflowStrip';
import { TraceDrawer } from '@/features/chat/TracePanel';
import { ContextPanel } from '@/features/chat/ContextPanel';
import { ChatTopBar } from '@/features/chat/ChatTopBar';
import { NewProjectDialog, useProjectMenu } from '@/features/chat/ProjectControls';
import { ArrowDownIcon, FileDirectoryIcon } from '@primer/octicons-react';
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
  scope?: string;
  // The behavior group, read for the Vision flag (image input gate).
  behavior?: { vision?: boolean };
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
  onSend: (text: string, agentConfigId: string, projectId?: string, attachments?: AttachmentMeta[]) => void;
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
  // Opens the Settings dialog, optionally on a named tab (e.g. 'agents').
  onSettingsOpen?: (tab?: string) => void;
  onPanelChange: (panel: InspectorPanel) => void;
  // Opens the global terminal panel (app-level, independent of the session).
  // Open-only by design: closing/collapsing happens on the panel itself. The
  // bound session's project is passed along, and a freshly opened panel
  // starts a terminal for it — in the same container the session's runs use.
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
    liveRunId, tasks, tasksLoaded, tasksError, taskView, hasMore, loadingMore,
  } = state;
  const {
    onSend, onCancel, onApprove, onReject, onFork, onLoadEarlier, onSwitchBranch, onCompact, onRegenerate,
    onWatchTask, onUnwatchTask, onPatchTask, onLoadSpan, onPanelChange, onTerminalOpen, onSettingsOpen,
  } = actions;
  const [agentConfigId, setAgentConfigIdState] = useState(() => loadSessionAgent(sessionId || ''));
  const [projectId, setProjectIdState] = useState(() => loadSessionProject(sessionId || ''));
  // The "New project…" dialog, seeded with the machine to offer first.
  const [newProject, setNewProject] = useState<{ sandboxId: string } | null>(null);

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
  const { data: agentConfigs, reload: reloadAgents } = useApi<AgentConfig[]>(() => api.agents.list() as Promise<AgentConfig[]>, [], 'agents');
  const { data: sandboxDefs, reload: reloadSandboxes } = useApi<SandboxDef[]>(() => api.sandboxes.list() as Promise<SandboxDef[]>, [], 'sandboxes');
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
    // No session has no server-side agent to wait for; a session's is still loading.
    if (sessionId && sessionAgentId === undefined) return;
    if (sessionAgentId && agentConfigs.some(a => a.id === sessionAgentId)) {
      setAgentConfigId(sessionAgentId); // the session's server-side agent
      return;
    }
    // A new, unbound conversation: reopen on the last agent the user picked,
    // then fall back to the first in the list.
    const last = loadLastAgent();
    setAgentConfigId(agentConfigs.some(a => a.id === last) ? last : agentConfigs[0].id);
  }, [agentConfigs, agentConfigId, sessionId, sessionAgentId, setAgentConfigId]);

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

  // The code-block copy buttons live in rendered markdown, outside React, so
  // the click is caught here and the button's icon flipped by hand.
  const { copy } = useCopy();
  const handleCopyClick = useCallback((e: MouseEvent<HTMLDivElement>) => {
    const expand = (e.target as HTMLElement).closest('.btn-code-expand') as HTMLElement | null;
    if (expand) {
      expand.closest('.code-block-wrapper')?.classList.remove('code-collapsed');
      return;
    }
    const btn = (e.target as HTMLElement).closest('.btn-copy') as HTMLElement | null;
    if (!btn) return;
    // getAttribute returns the decoded value; the renderer escaped it.
    const code = btn.getAttribute('data-code');
    if (!code) return;
    void copy(code).then(ok => {
      if (!ok) return;
      btn.classList.add('copied');
      const svgContent = btn.innerHTML;
      btn.innerHTML = CHECK_ICON;
      setTimeout(() => { btn.classList.remove('copied'); btn.innerHTML = svgContent; }, 1500);
    });
  }, [copy]);

  const selectedProject = projects?.find(p => p.id === projectId);
  const sandboxName = (id?: string) => sandboxDefs?.find(sb => sb.id === id)?.name || '';
  // The bound pair is what the top-bar menu acts on: a session's container is
  // its binding's, never the composer's current pick.
  const boundProject = sessionBinding?.projectId ? projects?.find(p => p.id === sessionBinding.projectId) || null : null;
  const boundSandbox = sandboxDefs?.find(sb => sb.id === boundProject?.sandbox_id);
  // Capabilities come from the sandbox row the project names. Until that row
  // declares `rebuild`, Rebuild is withheld rather than offered on a guess: a
  // backend whose store IS the compute cannot be rebuilt.
  const { menu: projectMenu, dialog: envDialog } = useProjectMenu({
    project: boundProject,
    rebuildable: !!boundProject && !!boundSandbox?.supports?.rebuild,
    running,
    onProjectsChanged: reloadProjects,
  });
  const sandboxView = composerSandboxView(sessionBinding || null, projects, sandboxDefs);

  const handleSend = useCallback((text: string, attachments?: AttachmentMeta[]) => {
    // No sessionId is fine: sending with no active session starts a new session
    // (app-level onSend auto-creates it). Only an agent is required.
    if (!agentConfigId) return;
    // Bound: the server uses the binding regardless — send no project claim.
    onSend(text, agentConfigId, sessionBinding ? '' : projectId, attachments);
  }, [agentConfigId, projectId, sessionBinding, onSend]);

  // Image affordances follow the PICKED agent's Vision flag; the server
  // re-checks at run start, so this is presentation, not the gate.
  const allowAttachments = Boolean(agentConfigs?.find(a => a.id === agentConfigId)?.behavior?.vision);


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
              // The turn's run OVERWRITES the one the user message carries:
              // a message's own run_id is whichever run first produced it —
              // after a regenerate, an attempt the session has branched away
              // from. On the active branch a message is followed by exactly
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
  // The lineage lives on the run's own durable output — task rows and
  // notification text do not survive a fork or a fold.
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
    () => ({ sessionId, running, compacting, diagnostics, agentAvatars, tasksError }),
    [sessionId, running, compacting, agentAvatars, diagnostics, tasksError],
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
      // The bound project's terminal — the same container the runs use. The
      // menu that offers it renders only once the session is bound.
      onTerminalOpen={onTerminalOpen && boundProject
        ? () => onTerminalOpen({ projectId: boundProject.id, projectName: boundProject.name, targetName: sandboxName(boundProject.sandbox_id) })
        : undefined}
      binding={sandboxView.bound && sessionBinding
        ? { title: sandboxView.title, projectName: projects?.find(p => p.id === sessionBinding.projectId)?.name || '…' }
        : null}
      projectMenu={projectMenu}
    />
  );

  // Every branch below renders inside the session scope — the top bar reads it
  // even with no session open. No hooks past this point.
  const scoped = (body: ReactNode) => (
    <ChatSessionProvider session={session} actions={turnActions} tasks={chatTasks}>{body}</ChatSessionProvider>
  );

  const selectedAgent = agentConfigs?.find(a => a.id === agentConfigId);
  const selectedAgentLabel = selectedAgent?.name || 'Agent';
  const agentCollisions = collidingNames(agentConfigs || []);

  // The composer's "+" carries the Project submenu until the session binds:
  // the caller's projects newest first, then New project. The pick shows only
  // here — checked in the list, named on the Project row — and picking the
  // checked project again clears it. Once bound, the top bar's badge is the
  // binding and the submenu is gone.
  const projectRows = composerProjectRows(projects, sandboxDefs);
  const plusItems: ReactNode = !sandboxView.bound && sandboxDefs && sandboxDefs.length > 0 ? (
    <ActionMenu>
      <ActionMenu.Anchor>
        <ActionList.Item>
          <ActionList.LeadingVisual><FileDirectoryIcon /></ActionList.LeadingVisual>
          Project
          {selectedProject && <ActionList.Description variant="inline">{selectedProject.name}</ActionList.Description>}
        </ActionList.Item>
      </ActionMenu.Anchor>
      <ActionMenu.Overlay>
        <ActionList selectionVariant="single">
          {/* A failed fetch must not read as an empty account. */}
          {projectsError ? (
            <ActionList.Item disabled>projects failed to load</ActionList.Item>
          ) : projectRows.length === 0 ? (
            <ActionList.Item disabled>no projects yet</ActionList.Item>
          ) : projectRows.map(({ project: p, sandboxName: sb }) => (
            <ActionList.Item key={p.id} selected={projectId === p.id} onSelect={() => setProjectId(projectId === p.id ? '' : p.id)} title={projectLabel(p.name, sb)}>
              {p.name}
              <ActionList.Description variant="inline">{sb}</ActionList.Description>
            </ActionList.Item>
          ))}
          <ActionList.Divider />
          <ActionList.Item onSelect={() => setNewProject({ sandboxId: selectedProject?.sandbox_id || sandboxDefs[0].id })}>
            New project…
          </ActionList.Item>
        </ActionList>
      </ActionMenu.Overlay>
    </ActionMenu>
  ) : null;

  const inputToolbar: ReactNode = (
    <>
      {newProject && sandboxDefs && (
        <NewProjectDialog
          sandboxes={sandboxDefs}
          initialSandboxId={newProject.sandboxId}
          onClose={() => setNewProject(null)}
          onCreated={created => {
            // Seed the cached list before selecting: the stale-id guard
            // above runs against `projects` on the very next commit, and a
            // fire-and-forget reload would hand it a list without the new
            // row — wiping the selection it should protect.
            mutateProjects(prev => prev ? [...prev.filter(p => p.id !== created.id), created] : [created]);
            if (created.id) setProjectId(created.id);
            reloadProjects();
            setNewProject(null);
          }}
        />
      )}
      {envDialog}
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
                    {agentCollisions.has(a.name) && (
                      <ActionList.TrailingVisual><ScopeHint agent={a} colliding={agentCollisions} /></ActionList.TrailingVisual>
                    )}
                  </ActionList.Item>
                ))}
              </ActionList>
            </ActionMenu.Overlay>
          </ActionMenu>
        ) : (
          <span className="chat-input-toolbar-warn">
            No agents — {onSettingsOpen
              ? <Link as="button" type="button" onClick={() => onSettingsOpen('agents')}>add one in Settings</Link>
              : 'add one in Settings'}
          </span>
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
            attachments={(m as { attachments?: AttachmentMeta[] }).attachments}
            traceRunId={rid || null}
            msgIdx={i}
          />
        );
      }
      if (m.role === 'compaction') {
        return <CompactionCard key={entryKey(m, i, 'compaction')} content={m.content} tokensBefore={m.tokensBefore} tokensAfter={m.tokensAfter} />;
      }
      if (m.role === 'system' && m.note) {
        return <WorkflowStartedChip key={entryKey(m, i, 'msg')} note={m.note} content={m.content || ''} traceRunId={userRunMap[i] || null} msgIdx={i} />;
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


  // A selected session whose timeline is still loading. No session has nothing
  // to load, so it falls through to the composer below.
  if (sessionId && !loaded && messages.length === 0) {
    return scoped(
      <div className="chat-main">
        <div className="chat-content">
          {topBar}
          <Loading kind="panel" />
        </div>
      </div>
    );
  }

  // No session yet, or an empty one: the same centered composer. Typing and
  // sending with no session creates one (app-level handleSend), so the blank
  // screen is a place to start, not a dead end.
  if (!sessionId || isEmpty) {
    return scoped(
      <div className={'chat-main' + (panel && sessionId ? ' trace-open' : '')}>
        <div className="chat-content">
          {topBar}
          <div className="chat-content chat-content-centered">
            <Greeting key={`greeting-${sessionId || 'new'}`} />
            <WorkflowStrip />
            <MessageInput
              key={`input-${sessionId || 'new'}`}
              sessionId={sessionId || ''}
              onSend={handleSend}
              onCancel={handleCancel}
              disabled={running || awaiting || !agentConfigId}
              running={running}
              allowAttachments={allowAttachments}
              toolbar={inputToolbar}
      plusItems={plusItems}
            />
          </div>
        </div>
        {sessionId ? sidePanels : null}
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
          allowAttachments={allowAttachments}
          toolbar={inputToolbar}
      plusItems={plusItems}
        />
      </div>

      {sidePanels}
    </div>
  );
}
