import { createContext, useContext, useRef, type Context, type ReactNode } from 'react';
import type { RunDiagnostic } from '@/lib/protocol';
import { backgroundItems, type BackgroundItem } from '@/lib/background';
import { taskRetryable, type TaskState } from '@/lib/useAgentSocket';

// The chat's session scope, split four ways by how often each value moves,
// because a context has no selectors — every consumer re-renders when its
// value changes:
//   - ChatSessionState: the run lifecycle. Flips per run, never per delta.
//   - ChatActions: callbacks. Memoized upstream, so the value only changes on a
//     session switch or a picker change.
//   - ChatTaskLookups: the per-call maps the tool cards read. Identity-stable:
//     it moves only when a lookup's CONTENT changes, not on every task event
//     (a child run's every tool call patches the task's lastTool).
//   - the background items: the list the strip, the Tasks panel and the top
//     bar read. Moves per task event, which is what those show.
// What changes per streaming delta (streaming, reasoning, the live turn's
// parts) is deliberately NOT here: it stays a prop of the one live TurnBlock,
// which is what keeps a delta from re-rendering a finished turn.

export interface ChatSessionState {
  sessionId: string | null;
  running: boolean;
  compacting: boolean;
  // Agent-config id → avatar path, for the parts that name an agent by id
  // (handoffs, trigger notes). Only configs WITH an avatar appear.
  agentAvatars: Record<string, string>;
  // Trouble the live run survived, badged on its process group.
  diagnostics?: RunDiagnostic[];
  // Set when the durable task list failed to load, so the Tasks panel says so
  // instead of showing "no background work".
  tasksError?: string;
}

export interface ChatActions {
  approve?: (toolCallId: string, scope?: string) => void;
  reject?: (toolCallId: string) => void;
  fork?: (messageId: string) => void;
  switchBranch?: (tipEntryId: string) => void;
  // Branches back to the user ENTRY id and runs again.
  regenerate?: (userEntryId: string, userContent: string) => void;
  openTrace: (runId: string) => void;
  inspectTask: (taskId: string) => void;
  retryTask: (taskId: string) => Promise<void>;
  stopTask: (taskId: string) => Promise<void>;
  dismissTask: (taskId: string) => Promise<void>;
  // Loads one trace span's payload — left out of the listing the panel opened
  // with — from the session whose stored rows hold it (the chat's own, or an
  // inspected task's child).
  loadSpan?: (spanSessionId: string, runId: string, spanId: string) => Promise<void>;
}

export interface ChatTaskLookups {
  // toolCallId → whether the server would accept a retry of the task that call
  // spawned ("failed" alone says nothing about attempts left).
  retryableByCallId: Record<string, boolean>;
  // toolCallId → live status (working/input_required) from run events; the
  // terminal status comes from the display projection on the call itself.
  liveTaskStatusByCallId: Record<string, string>;
  // toolCallId → the spawn label, for the card header before the terminal
  // display projection lands.
  liveTaskLabelByCallId: Record<string, string>;
  // taskId → label, so task_status / task_stop cards name the task they act on.
  taskLabelById: Record<string, string>;
}

export interface ChatTasks {
  // The session's background work as one list.
  items: BackgroundItem[];
  lookups: ChatTaskLookups;
}

function sameEntries<V>(a: Record<string, V>, b: Record<string, V>): boolean {
  const ak = Object.keys(a);
  return ak.length === Object.keys(b).length && ak.every(k => a[k] === b[k]);
}

// deriveChatTasks builds the task context value from the socket's task state.
// Given the previous value, each lookup keeps its identity while its entries
// are unchanged, and so does the lookups object — the memoized tool cards
// then hold through the task events that change nothing they show.
export function deriveChatTasks(tasks: Record<string, TaskState> | undefined, prev?: ChatTasks): ChatTasks {
  const next: ChatTaskLookups = { retryableByCallId: {}, liveTaskStatusByCallId: {}, liveTaskLabelByCallId: {}, taskLabelById: {} };
  for (const t of Object.values(tasks || {})) {
    if (t.toolCallId) {
      if (taskRetryable(t)) next.retryableByCallId[t.toolCallId] = true;
      if (t.status === 'working' || t.status === 'input_required') next.liveTaskStatusByCallId[t.toolCallId] = t.status;
      if (t.label) next.liveTaskLabelByCallId[t.toolCallId] = t.label;
    }
    if (t.taskId && t.label) next.taskLabelById[t.taskId] = t.label;
  }
  let lookups = next;
  if (prev) {
    const p = prev.lookups;
    if (sameEntries(next.retryableByCallId, p.retryableByCallId)) next.retryableByCallId = p.retryableByCallId;
    if (sameEntries(next.liveTaskStatusByCallId, p.liveTaskStatusByCallId)) next.liveTaskStatusByCallId = p.liveTaskStatusByCallId;
    if (sameEntries(next.liveTaskLabelByCallId, p.liveTaskLabelByCallId)) next.liveTaskLabelByCallId = p.liveTaskLabelByCallId;
    if (sameEntries(next.taskLabelById, p.taskLabelById)) next.taskLabelById = p.taskLabelById;
    if (next.retryableByCallId === p.retryableByCallId && next.liveTaskStatusByCallId === p.liveTaskStatusByCallId &&
      next.liveTaskLabelByCallId === p.liveTaskLabelByCallId && next.taskLabelById === p.taskLabelById) lookups = p;
  }
  return { items: backgroundItems(tasks), lookups };
}

// useDerivedChatTasks is deriveChatTasks with the previous value remembered,
// for the one caller that feeds the provider.
export function useDerivedChatTasks(tasks: Record<string, TaskState> | undefined): ChatTasks {
  const ref = useRef<{ tasks: Record<string, TaskState> | undefined; value: ChatTasks } | null>(null);
  if (!ref.current || ref.current.tasks !== tasks) {
    ref.current = { tasks, value: deriveChatTasks(tasks, ref.current?.value) };
  }
  return ref.current.value;
}

const SessionContext = createContext<ChatSessionState | null>(null);
const ActionsContext = createContext<ChatActions | null>(null);
const LookupsContext = createContext<ChatTaskLookups | null>(null);
const ItemsContext = createContext<BackgroundItem[] | null>(null);

interface ChatSessionProviderProps {
  session: ChatSessionState;
  actions: ChatActions;
  tasks: ChatTasks;
  children: ReactNode;
}

// The values must be memoized by the caller (ChatView): a fresh object per
// render would re-render every consumer on every delta.
export function ChatSessionProvider({ session, actions, tasks, children }: ChatSessionProviderProps) {
  return (
    <SessionContext.Provider value={session}>
      <ActionsContext.Provider value={actions}>
        <LookupsContext.Provider value={tasks.lookups}>
          <ItemsContext.Provider value={tasks.items}>{children}</ItemsContext.Provider>
        </LookupsContext.Provider>
      </ActionsContext.Provider>
    </SessionContext.Provider>
  );
}

function useRequiredContext<T>(ctx: Context<T | null>, hook: string): T {
  const value = useContext(ctx);
  if (value === null) throw new Error(hook + ' must be used inside ChatSessionProvider');
  return value;
}

export function useChatSession(): ChatSessionState {
  return useRequiredContext(SessionContext, 'useChatSession');
}

export function useChatActions(): ChatActions {
  return useRequiredContext(ActionsContext, 'useChatActions');
}

// useChatTaskLookups is what a tool card reads: the per-call maps, moving only
// when a map's content does.
export function useChatTaskLookups(): ChatTaskLookups {
  return useRequiredContext(LookupsContext, 'useChatTaskLookups');
}

// useChatBackground is the session's background work as one list, moving on
// every task event.
export function useChatBackground(): BackgroundItem[] {
  return useRequiredContext(ItemsContext, 'useChatBackground');
}
