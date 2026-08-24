import './chat.css';
import { useState, useEffect, useCallback, useMemo, useRef, type MouseEvent, type ReactNode } from 'react';
import { Button, Dialog, IconButton, ActionMenu, ActionList, Select, Stack, TextInput } from '@primer/react';
import { Blankslate } from '@primer/react/experimental';
import { api } from '@/lib/api';
import { CHECK_ICON } from '@/lib/markdownShared';
import { type TurnPart, type TimelineEntry, type Branches, type WorkflowStartedNote } from '@/lib/timeline';
import { useScrollToBottom, useApi } from '@/lib/hooks';
import { loadSessionAgent, saveSessionAgent, loadSessionSandbox, saveSessionSandbox, loadSessionProject, saveSessionProject } from '@/lib/drafts';
import { composerSandboxView, groupProjects, projectLabel, type Project, type SessionBinding } from '@/lib/binding';
import { useProjects } from '@/lib/useProjects';
import { fc } from '@/lib/form';
import { parseTaskNotification, TASK_KIND_WORKFLOW, type TaskStatus } from '@/lib/protocol';
import type { SessionState, TaskState } from '@/lib/useAgentSocket';
import { ChatSessionProvider, useDerivedChatTasks, type ChatSessionState, type ChatActions } from '@/features/chat/ChatSessionContext';
import { BackgroundListPanel, BackgroundDetailPanel, BackgroundMissingPanel } from '@/features/chat/BackgroundPanel';
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
import { ArrowDownIcon, CommentDiscussionIcon, DependabotIcon, FileDirectoryIcon, PlusIcon } from '@primer/octicons-react';
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
}

interface SandboxConfig {
  id: string;
  name: string;
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
  onSend: (text: string, agentConfigId: string, sandboxId: string, projectId?: string) => void;
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
  onRegenerate?: (userEntryId: string, userContent: string, agentConfigId: string, sandboxId: string, projectId?: string) => void;
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
  // the session is bound to a sandbox its (sandbox, project) is passed along,
  // and a freshly opened panel starts a terminal for it — in the same
  // container the session's runs use.
  onTerminalOpen?: (sandbox?: { id: string; name: string; projectId: string; projectName?: string }) => void;
}

interface ChatViewProps {
  sessionId: string | null;
  // The session's display name for the top bar ('' until known).
  sessionName?: string;
  // The session's permanent (sandbox, project) binding, or null while unbound.
  // Set by the first sandbox-carrying run; server-authoritative and immutable
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
  sessionId, sessionName, sessionBinding, state, awaiting, settingsReloadKey, bindingsVersion, panel, actions,
}: ChatViewProps) {
  // The rendered timeline drops the entries no longer on the active branch;
  // the trace panel still lists their runs, so it reads the raw entries.
  const messages: ChatMessage[] = state.messages;
  const {
    entries, loaded, streaming, reasoning, running, compacting, diagnostics, traceRuns, runQuestions,
    liveRunId, liveStartedAt, liveAgentName, tasks, tasksLoaded, taskView, hasMore, loadingMore,
  } = state;
  const {
    onSend, onCancel, onApprove, onReject, onFork, onLoadEarlier, onSwitchBranch, onCompact, onRegenerate,
    onWatchTask, onUnwatchTask, onPatchTask, onLoadSpan, onPanelChange, onTerminalOpen,
  } = actions;
  const [agentConfigId, setAgentConfigIdState] = useState(() => loadSessionAgent(sessionId || ''));
  const [sandboxId, setSandboxIdState] = useState(() => loadSessionSandbox(sessionId || ''));
  const [projectId, setProjectIdState] = useState(() => loadSessionProject(sessionId || ''));
  // The "New project…" dialog: pick a sandbox, name the project.
  const [projDialogOpen, setProjDialogOpen] = useState(false);
  const [projSandboxId, setProjSandboxId] = useState('');
  const [projName, setProjName] = useState('');
  const [projSaving, setProjSaving] = useState(false);

  useEffect(() => {
    setAgentConfigIdState(loadSessionAgent(sessionId || ''));
    setSandboxIdState(loadSessionSandbox(sessionId || ''));
    setProjectIdState(loadSessionProject(sessionId || ''));
  }, [sessionId]);

  const setAgentConfigId = useCallback((id: string) => {
    setAgentConfigIdState(id);
    saveSessionAgent(sessionId || '', id);
  }, [sessionId]);

  const setProjectId = useCallback((id: string) => {
    setProjectIdState(id);
    saveSessionProject(sessionId || '', id);
  }, [sessionId]);

  const setSandboxId = useCallback((id: string) => {
    setSandboxIdState(id);
    saveSessionSandbox(sessionId || '', id);
    // A project chosen for sandbox A must not silently apply to sandbox B.
    setProjectIdState('');
    saveSessionProject(sessionId || '', '');
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
  const { data: sandboxConfigs, reload: reloadSandboxes } = useApi<SandboxConfig[]>(() => api.sandboxes.list() as Promise<SandboxConfig[]>);
  // The caller's project rows for the picker — the same hook the terminal
  // panel's + menu uses.
  const { projects, error: projectsError, reload: reloadProjects, mutate: mutateProjects } = useProjects(bindingsVersion);

  useEffect(() => {
    if (!agentConfigs || agentConfigs.length === 0) return;
    const valid = agentConfigs.some(a => a.id === agentConfigId);
    if (!valid) {
      setAgentConfigId(agentConfigs[0].id);
    }
  }, [agentConfigs, agentConfigId, setAgentConfigId]);

  // A persisted sandbox may have since been deleted: drop a now-unknown id
  // back to None ('' is a valid choice), so the composer doesn't carry a stale
  // sandbox_id and the label doesn't fall back to a generic "Sandbox".
  useEffect(() => {
    if (!sandboxId || !sandboxConfigs) return;
    if (!sandboxConfigs.some(s => s.id === sandboxId)) setSandboxId('');
  }, [sandboxConfigs, sandboxId, setSandboxId]);

  // Same for a persisted project id whose row is gone — a send carrying it
  // would be refused.
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

  const selectedSandbox = sandboxConfigs?.find(s => s.id === sandboxId);
  const selectedProject = projects?.find(p => p.id === projectId);
  const sandboxView = composerSandboxView(sessionBinding || null, projects, sandboxConfigs);

  const handleSend = useCallback((text: string) => {
    // No sessionId is fine: sending with no active session starts a new session
    // (app-level onSend auto-creates it). Only an agent is required.
    if (!agentConfigId) return;
    if (sessionBinding) {
      // Bound: the server uses the binding regardless — send no sandbox claim.
      onSend(text, agentConfigId, '', '');
      return;
    }
    // An empty projectId beside a sandbox is valid: the server uses the
    // per-user scratch project on that sandbox.
    onSend(text, agentConfigId, sandboxId, projectId);
  }, [agentConfigId, sandboxId, projectId, sessionBinding, onSend]);


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

  // Bound sessions claim no sandbox (the server uses the binding); unbound ones
  // carry the choice, because a regen can be the first sandbox-carrying run.
  const regenSandboxId = sessionBinding ? '' : sandboxId;
  const regenProjectId = sessionBinding ? '' : projectId;
  const handleRegen = useCallback((messageId: string, content: string) => {
    onRegenerate?.(messageId, content, agentConfigId, regenSandboxId, regenProjectId);
  }, [onRegenerate, agentConfigId, regenSandboxId, regenProjectId]);

  // The session scope every transcript component reads (see
  // ChatSessionContext for the split). Each value is memoized on its inputs so
  // a streaming delta — which changes none of them — re-renders no consumer.
  const session = useMemo<ChatSessionState>(
    () => ({ sessionId, running, compacting, liveAgentName, liveStartedAt, diagnostics }),
    [sessionId, running, compacting, liveAgentName, liveStartedAt, diagnostics],
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
      terminalEnabled={!!onTerminalOpen && !!sandboxConfigs && sandboxConfigs.length > 0}
      onTerminalOpen={onTerminalOpen
        ? () => {
          // A bound session's terminal follows its binding — the same
          // (sandbox, project) container the runs use. Unbound sessions fall
          // back to the picker's current selection, then the sandbox's first
          // project; a terminal cannot open without a project.
          const bound = sessionBinding ? sandboxConfigs?.find(s => s.id === sessionBinding.sandboxId) : undefined;
          const nameOf = (pid: string) => projects?.find(p => p.id === pid)?.name || '';
          if (bound && sessionBinding?.projectId) {
            onTerminalOpen({ id: bound.id, name: bound.name, projectId: sessionBinding.projectId, projectName: nameOf(sessionBinding.projectId) });
          } else if (!sessionBinding && selectedSandbox) {
            const pid = projectId || projects?.find(p => p.sandbox_id === selectedSandbox.id)?.id;
            onTerminalOpen(pid ? { id: selectedSandbox.id, name: selectedSandbox.name, projectId: pid, projectName: nameOf(pid) } : undefined);
          } else {
            onTerminalOpen(undefined);
          }
        }
        : undefined}
      binding={sandboxView.bound && sessionBinding
        ? { title: sandboxView.title, projectName: projects?.find(p => p.id === sessionBinding.projectId)?.name || '…' }
        : null}
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

  const selectedAgentLabel = agentConfigs?.find(a => a.id === agentConfigId)?.name || 'Agent';

  const inputToolbar: ReactNode = (
    <>
      <div className="chat-input-toolbar-left">
        {/* Bound sessions show nothing here — the binding lives in the top
            bar's badge. Before binding the picker offers PROJECTS — the
            caller's rows, grouped by sandbox — because the project is what a
            person recognizes; the backend is its attribute, not the other
            way around. */}
        {!sandboxView.bound && sandboxConfigs && sandboxConfigs.length > 0 && (
          <ActionMenu>
            {/* Nothing picked yet reads as an offer, "+", not as a folder
                that is not there; a picked project shows as itself. */}
            {sandboxId && selectedSandbox ? (
              <ActionMenu.Button size="small" variant="invisible" leadingVisual={FileDirectoryIcon}>
                {selectedProject ? projectLabel(selectedProject.name, selectedSandbox.name) : selectedSandbox.name}
              </ActionMenu.Button>
            ) : (
              <ActionMenu.Anchor>
                <IconButton icon={PlusIcon} size="small" variant="invisible" aria-label="Project" />
              </ActionMenu.Anchor>
            )}
            <ActionMenu.Overlay>
              <ActionList selectionVariant="single">
                <ActionList.Item selected={sandboxId === ''} onSelect={() => setSandboxId('')}>
                  None
                  <ActionList.Description variant="inline">chat only</ActionList.Description>
                </ActionList.Item>
                {/* A failed fetch must not read as an empty account. */}
                {projectsError && <ActionList.Item disabled>projects failed to load</ActionList.Item>}
                {/* One group per sandbox: the group heading carries the
                    backend, rows carry just the project name. */}
                {groupProjects(projects, sandboxConfigs).map(g => (
                  <ActionList.Group key={g.sandboxId}>
                    {/* Primer requires an explicit heading level on list-role
                        ActionLists; omitting `as` throws and unmounts the app. */}
                    <ActionList.GroupHeading as="h3">{g.sandboxName}</ActionList.GroupHeading>
                    {g.items.map(p => (
                      <ActionList.Item
                        key={p.id}
                        selected={projectId === p.id}
                        onSelect={() => { setSandboxId(p.sandbox_id); setProjectId(p.id); }}
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
                    setProjSandboxId((selectedSandbox || sandboxConfigs[0]).id);
                    setProjName('');
                    setProjDialogOpen(true);
                  }}
                >
                  New project…
                </ActionList.Item>
              </ActionList>
            </ActionMenu.Overlay>
          </ActionMenu>
        )}
        {projDialogOpen && (() => {
          const projSandbox = sandboxConfigs?.find(s => s.id === projSandboxId);
          const create = async () => {
            if (!projSandbox || !projName.trim() || projSaving) return;
            setProjSaving(true);
            try {
              const created = await api.projects.create({ name: projName.trim(), sandbox_id: projSandbox.id }) as Project;
              // Seed the cached list before selecting: the stale-id guard
              // below runs against `projects` on the very next commit, and a
              // fire-and-forget reload would hand it a list without the new
              // row — wiping the selection it should protect.
              mutateProjects(prev => prev ? [...prev.filter(p => p.id !== created.id), created] : [created]);
              setSandboxId(projSandbox.id);
              if (created.id) setProjectId(created.id);
              reloadProjects();
              setProjDialogOpen(false);
            } catch (e) {
              // 409: a project of that name already exists on this sandbox.
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
                  disabled: !projSandbox || !projName.trim() || projSaving,
                  onClick: () => { void create(); },
                },
              ]}
            >
              <Stack gap="normal">
                {fc('Sandbox', (
                  <Select
                    block
                    value={projSandboxId}
                    onChange={e => setProjSandboxId(e.target.value)}
                  >
                    {sandboxConfigs?.map(s => (
                      <Select.Option key={s.id} value={s.id}>{s.name}</Select.Option>
                    ))}
                  </Select>
                ), '')}
                {fc('Name', (
                  <TextInput
                    block
                    value={projName}
                    placeholder="e.g. goagents"
                    onChange={e => setProjName(e.target.value)}
                  />
                ), 'Names the working tree the sandbox mounts; unique per sandbox.')}
              </Stack>
            </Dialog>
          );
        })()}
      </div>
      <div className="chat-input-toolbar-right">
        {agentConfigs && agentConfigs.length > 0 ? (
          <ActionMenu>
            <ActionMenu.Button size="small" variant="invisible" leadingVisual={DependabotIcon}>
              {selectedAgentLabel}
            </ActionMenu.Button>
            <ActionMenu.Overlay>
              <ActionList selectionVariant="single">
                {agentConfigs.map(a => (
                  <ActionList.Item key={a.id} selected={agentConfigId === a.id} onSelect={() => setAgentConfigId(a.id)}>
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
