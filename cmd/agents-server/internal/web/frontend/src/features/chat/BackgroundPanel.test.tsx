// @vitest-environment jsdom
import { describe, it, expect, beforeAll, afterAll, vi } from 'vitest';
import { act, type ReactNode } from 'react';
import { createRoot } from 'react-dom/client';

// The detail lens is mounted with its chrome stubbed: Primer, the side panel,
// the markdown worker (text passes through) and the trace waterfall. What is
// under test is the checklist block above the transcript.
const { memoryKey } = vi.hoisted(() => ({ memoryKey: vi.fn() }));
vi.mock('@primer/react', () => ({
  Button: ({ children, onClick }: { children?: ReactNode; onClick?: () => void }) => <button type="button" onClick={onClick}>{children}</button>,
  IconButton: ({ 'aria-label': label, onClick }: { 'aria-label'?: string; onClick?: () => void }) => <button type="button" aria-label={label} onClick={onClick} />,
  Label: ({ children }: { children?: ReactNode }) => <span>{children}</span>,
  useConfirm: () => async () => false,
}));
vi.mock('@primer/octicons-react', () => Object.fromEntries(
  ['ArrowLeftIcon', 'StackIcon', 'CopyIcon', 'CheckIcon', 'WorkflowIcon', 'DotFillIcon', 'CircleIcon', 'ChecklistIcon'].map(n => [n, () => null]),
));
vi.mock('@/layout/SidePanel', () => ({ SidePanel: ({ children }: { children?: ReactNode }) => <div>{children}</div> }));
vi.mock('@/components/Loading', () => ({ Loading: () => <div>loading</div> }));
vi.mock('@/components/LoadError', () => ({ LoadError: () => null }));
vi.mock('@/components/AgentAvatar', () => ({ AgentAvatar: () => null }));
vi.mock('@/features/chat/ToolCallCard', () => ({ ToolCallCard: () => null }));
vi.mock('@/features/chat/StreamingMarkdown', () => ({ StreamingMarkdown: ({ text }: { text: string }) => <div>{text}</div> }));
vi.mock('@/features/chat/TracePanel', () => ({ TraceRun: () => null }));
vi.mock('@/features/chat/RejectButton', () => ({ RejectButton: () => null }));
vi.mock('@/lib/markdown', () => ({ useAsyncMarkdown: (text: string) => text }));
vi.mock('@/lib/toast', () => ({ toast: { success: () => {}, error: () => {} } }));
vi.mock('@/lib/api', () => ({ api: { sessions: { memoryKey }, workflows: {} } }));
import { ChatSessionProvider, deriveChatTasks, type ChatActions, type ChatSessionState } from '@/features/chat/ChatSessionContext';
import { BackgroundDetailPanel } from '@/features/chat/BackgroundPanel';
import type { BackgroundItem } from '@/lib/background';
import type { TaskViewState } from '@/lib/useAgentSocket';

const g = globalThis as Record<string, unknown>;
let savedActEnv: unknown;
beforeAll(() => { savedActEnv = g.IS_REACT_ACT_ENVIRONMENT; g.IS_REACT_ACT_ENVIRONMENT = true; });
afterAll(() => { if (savedActEnv === undefined) delete g.IS_REACT_ACT_ENVIRONMENT; else g.IS_REACT_ACT_ENVIRONMENT = savedActEnv; });

const SESSION: ChatSessionState = { sessionId: 'parent', running: false, compacting: false, agentAvatars: {} };
const noop = () => {};
const resolve = async () => {};
const actions: ChatActions = { openTrace: noop, inspectTask: noop, retryTask: resolve, stopTask: resolve, dismissTask: resolve };
const item: BackgroundItem = { kind: 'task', id: 't1', label: 'fix it', status: 'working', childSessionId: 'child1', retryable: false };
const view: TaskViewState = { taskId: 't1', childSessionId: 'child1', messages: [], streaming: '', reasoning: '', traceRuns: {}, loaded: true };

async function mount() {
  const host = document.createElement('div');
  document.body.appendChild(host);
  const root = createRoot(host);
  await act(async () => {
    root.render(
      <ChatSessionProvider session={SESSION} actions={actions} tasks={deriveChatTasks({})}>
        <BackgroundDetailPanel item={item} view={view} onBack={noop} onClose={noop} />
      </ChatSessionProvider>,
    );
  });
  await act(async () => {});
  return { host, unmount: () => { act(() => root.unmount()); host.remove(); } };
}

describe('BackgroundDetailPanel checklist', () => {
  // The task's own session holds the list; the lens shows it above the transcript.
  it('shows the child session checklist.md', async () => {
    memoryKey.mockResolvedValue({ content: '- [x] read it\n- [~] fix it (in progress)\n' });
    const m = await mount();
    expect(memoryKey).toHaveBeenCalledWith('child1', 'checklist.md');
    const block = m.host.querySelector('.task-view-checklist');
    expect(block?.textContent).toContain('Checklist');
    expect(block?.textContent).toContain('[~] fix it (in progress)');
    m.unmount();
  });

  it('shows nothing when the task keeps none', async () => {
    memoryKey.mockRejectedValue(new Error('404'));
    const m = await mount();
    expect(m.host.querySelector('.task-view-checklist')).toBeNull();
    expect(m.host.textContent).toContain('No transcript yet.');
    m.unmount();
  });
});
