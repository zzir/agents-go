// @vitest-environment jsdom
import { describe, it, expect, beforeAll, afterAll } from 'vitest';
import { act, memo, useMemo, useState } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import {
  ChatSessionProvider, deriveChatTasks, useDerivedChatTasks, useChatSession, useChatActions, useChatTaskLookups, useChatBackground,
  type ChatActions, type ChatSessionState,
} from '@/features/chat/ChatSessionContext';
import type { TaskState } from '@/lib/useAgentSocket';

const g = globalThis as Record<string, unknown>;
let savedActEnv: unknown;
beforeAll(() => {
  savedActEnv = g.IS_REACT_ACT_ENVIRONMENT;
  g.IS_REACT_ACT_ENVIRONMENT = true;
});
afterAll(() => {
  if (savedActEnv === undefined) delete g.IS_REACT_ACT_ENVIRONMENT; else g.IS_REACT_ACT_ENVIRONMENT = savedActEnv;
});

function mount(): { root: Root; container: HTMLElement } {
  const container = document.createElement('div');
  document.body.appendChild(container);
  return { root: createRoot(container), container };
}

/* ---------- fixtures ---------- */

const noop = () => {};
const resolve = async () => {};
const ACTIONS: ChatActions = { openTrace: noop, inspectTask: noop, retryTask: resolve, stopTask: resolve, dismissTask: resolve };
// A stand-in for a finished TurnBlock's tool card: memo'd, reads the session,
// the actions and the task LOOKUPS (not the background list), has no
// per-delta props.
let finishedRenders = 0;
const Finished = memo(function Finished() {
  finishedRenders++;
  const { running } = useChatSession();
  useChatActions();
  useChatTaskLookups();
  return <span>{running ? 'running' : 'idle'}</span>;
});

// A stand-in for the strip: reads the background list.
let stripRenders = 0;
const Strip = memo(function Strip() {
  stripRenders++;
  return <span>{useChatBackground().length}</span>;
});

// A stand-in for the live turn: the streaming text is a prop.
let liveRenders = 0;
const Live = memo(function Live({ streaming }: { streaming: string }) {
  liveRenders++;
  return <span>{streaming}</span>;
});

// Wires the provider the way ChatView does: values memoized on their inputs,
// the streaming text kept out of them.
let setStreaming: (s: string) => void = noop;
let setRunning: (r: boolean) => void = noop;
let setTasks: (t: Record<string, TaskState>) => void = noop;
function Harness() {
  const [streaming, _setStreaming] = useState('');
  const [running, _setRunning] = useState(false);
  const [tasks, _setTasks] = useState<Record<string, TaskState>>({});
  setStreaming = _setStreaming;
  setRunning = _setRunning;
  setTasks = _setTasks;
  const session = useMemo<ChatSessionState>(
    () => ({ sessionId: 's1', running, compacting: false, liveAgentName: null, liveAgentAvatar: null, liveStartedAt: null, agentAvatars: {} }),
    [running],
  );
  const chatTasks = useDerivedChatTasks(tasks);
  return (
    <ChatSessionProvider session={session} actions={ACTIONS} tasks={chatTasks}>
      <Finished />
      <Strip />
      <Live streaming={streaming} />
    </ChatSessionProvider>
  );
}

describe('ChatSessionContext', () => {
  it('hooks throw outside the provider', () => {
    const { root } = mount();
    function Orphan() { useChatSession(); return null; }
    expect(() => act(() => { root.render(<Orphan />); })).toThrow('useChatSession must be used inside ChatSessionProvider');
    act(() => { root.unmount(); });
  });

  it('a streaming delta re-renders the live turn only', () => {
    finishedRenders = 0;
    liveRenders = 0;
    const { root, container } = mount();
    act(() => { root.render(<Harness />); });
    expect(container.textContent).toBe('idle0');
    expect(finishedRenders).toBe(1);
    expect(liveRenders).toBe(1);

    act(() => { setStreaming('Hel'); });
    act(() => { setStreaming('Hello'); });
    expect(container.textContent).toBe('idle0Hello');
    expect(liveRenders).toBe(3);
    // The finished turn's memo held: nothing it reads changed.
    expect(finishedRenders).toBe(1);

    // A run-lifecycle change is what reaches it — once.
    act(() => { setRunning(true); });
    expect(container.textContent).toBe('running0Hello');
    expect(finishedRenders).toBe(2);
    expect(liveRenders).toBe(3);
    act(() => { root.unmount(); });
  });

  it('a task event that changes no lookup re-renders the strip, not the tool cards', () => {
    finishedRenders = 0;
    stripRenders = 0;
    const { root, container } = mount();
    act(() => { root.render(<Harness />); });
    expect(stripRenders).toBe(1);

    // A task appears: its label and live status are new lookups — both read it.
    act(() => { setTasks({ a: { taskId: 'a', label: 'Lint', status: 'working', toolCallId: 'c1' } }); });
    expect(container.textContent).toContain('1');
    expect(finishedRenders).toBe(2);
    expect(stripRenders).toBe(2);

    // The child run calls a tool: lastTool and updatedAt move, no lookup does.
    act(() => { setTasks({ a: { taskId: 'a', label: 'Lint', status: 'working', toolCallId: 'c1', lastTool: 'read_file', updatedAt: 1 } }); });
    expect(stripRenders).toBe(3);
    expect(finishedRenders).toBe(2);

    // The task ends: the live status lookup loses its entry — the cards hear.
    act(() => { setTasks({ a: { taskId: 'a', label: 'Lint', status: 'completed', toolCallId: 'c1', lastTool: 'read_file', updatedAt: 2 } }); });
    expect(finishedRenders).toBe(3);
    act(() => { root.unmount(); });
  });

  it('deriveChatTasks keys the lookups by call id and task id', () => {
    const tasks: Record<string, TaskState> = {
      a: { taskId: 'a', label: 'Lint', status: 'working', toolCallId: 'c1' },
      b: { taskId: 'b', label: 'Build', status: 'failed', toolCallId: 'c2', attempt: 1, maxAttempts: 3 },
      c: { taskId: 'c', label: '', status: 'completed', toolCallId: 'c3' },
    };
    const t = deriveChatTasks(tasks);
    expect(t.items.map(it => it.id)).toEqual(['a', 'b', 'c']);
    expect(t.lookups.liveTaskStatusByCallId).toEqual({ c1: 'working' });
    expect(t.lookups.retryableByCallId).toEqual({ c2: true });
    expect(t.lookups.liveTaskLabelByCallId).toEqual({ c1: 'Lint', c2: 'Build' });
    expect(t.lookups.taskLabelById).toEqual({ a: 'Lint', b: 'Build' });

    // Given the previous value, an unchanged lookup keeps its identity, and so
    // does the lookups object while none changed; one that changed is new.
    const same = deriveChatTasks({ ...tasks, a: { ...tasks.a, lastTool: 'grep' } }, t);
    expect(same.lookups).toBe(t.lookups);
    const moved = deriveChatTasks({ ...tasks, a: { ...tasks.a, status: 'completed' } }, t);
    expect(moved.lookups).not.toBe(t.lookups);
    expect(moved.lookups.liveTaskStatusByCallId).toEqual({});
    expect(moved.lookups.taskLabelById).toBe(t.lookups.taskLabelById);
    expect(moved.lookups.retryableByCallId).toBe(t.lookups.retryableByCallId);
  });
});
