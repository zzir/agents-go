// @vitest-environment jsdom
import { describe, it, expect, beforeAll, afterAll, vi } from 'vitest';
import { act, useMemo, useState, type ReactNode } from 'react';
import { createRoot } from 'react-dom/client';

// Primer ships CSS the node loader cannot import; the strip's two Primer
// pieces are plain elements here.
vi.mock('@primer/react', () => ({
  Button: ({ children, ...p }: { children?: ReactNode } & Record<string, unknown>) => <button {...(p as object)}>{children}</button>,
  Label: ({ children }: { children?: ReactNode }) => <span>{children}</span>,
}));
import { ChatSessionProvider, useDerivedChatTasks, type ChatActions, type ChatSessionState } from '@/features/chat/ChatSessionContext';
import { WorkflowStrip } from '@/features/chat/WorkflowStrip';
import type { TaskState } from '@/lib/useAgentSocket';

const g = globalThis as Record<string, unknown>;
let savedActEnv: unknown;
beforeAll(() => { savedActEnv = g.IS_REACT_ACT_ENVIRONMENT; g.IS_REACT_ACT_ENVIRONMENT = true; });
afterAll(() => { if (savedActEnv === undefined) delete g.IS_REACT_ACT_ENVIRONMENT; else g.IS_REACT_ACT_ENVIRONMENT = savedActEnv; });

const noop = () => {};
const resolve = async () => {};
const ACTIONS: ChatActions = { openTrace: noop, inspectTask: noop, retryTask: resolve, stopTask: resolve, dismissTask: resolve };

let setTasks: (t: Record<string, TaskState>) => void = noop;
function Harness() {
  const [tasks, _setTasks] = useState<Record<string, TaskState>>({});
  setTasks = _setTasks;
  const session = useMemo<ChatSessionState>(
    () => ({ sessionId: 's1', running: false, compacting: false, agentAvatars: {} }), []);
  const chatTasks = useDerivedChatTasks(tasks);
  return (
    <ChatSessionProvider session={session} actions={ACTIONS} tasks={chatTasks}>
      <WorkflowStrip />
    </ChatSessionProvider>
  );
}

describe('WorkflowStrip', () => {
  // The strip renders empty until a sequence starts, and every hook must run
  // in both states: a hook that only ran once a bar appeared changed the hook
  // order between the two renders — React #310 the moment /workflow started one.
  it('goes from empty to a live bar and back without changing its hooks', () => {
    const container = document.createElement('div');
    document.body.appendChild(container);
    const root = createRoot(container);
    act(() => { root.render(<Harness />); });
    expect(container.querySelector('.wf-bar')).toBeNull();

    act(() => { setTasks({ w: { taskId: 'w', label: 'build', kind: 'workflow', status: 'working', toolCallId: '' } }); });
    expect(container.querySelector('.wf-bar')).not.toBeNull();
    expect(container.textContent).toContain('build');

    act(() => { setTasks({ w: { taskId: 'w', label: 'build', kind: 'workflow', status: 'completed', toolCallId: '' } }); });
    expect(container.querySelector('.wf-bar')).toBeNull();
    act(() => { root.unmount(); });
  });
});
