// @vitest-environment jsdom
import { describe, it, expect, beforeAll, afterAll, vi } from 'vitest';
import { act, type ReactNode } from 'react';
import { createRoot } from 'react-dom/client';

vi.mock('@primer/react', () => ({
  Button: ({ children, onClick, 'aria-expanded': expanded }: { children?: ReactNode; onClick?: () => void; 'aria-expanded'?: boolean }) => <button type="button" aria-expanded={expanded} onClick={onClick}>{children}</button>,
}));
vi.mock('@primer/octicons-react', () => Object.fromEntries(['ChecklistIcon', 'CheckIcon', 'DotFillIcon', 'CircleIcon'].map(n => [n, () => null])));
import { ChatSessionProvider, deriveChatTasks, type ChatActions, type ChatSessionState } from '@/features/chat/ChatSessionContext';
import { ChecklistChip } from '@/features/chat/ChecklistChip';

const g = globalThis as Record<string, unknown>;
let savedActEnv: unknown;
beforeAll(() => { savedActEnv = g.IS_REACT_ACT_ENVIRONMENT; g.IS_REACT_ACT_ENVIRONMENT = true; });
afterAll(() => { if (savedActEnv === undefined) delete g.IS_REACT_ACT_ENVIRONMENT; else g.IS_REACT_ACT_ENVIRONMENT = savedActEnv; });

const noop = () => {};
const resolve = async () => {};
const actions: ChatActions = { openTrace: noop, inspectTask: noop, retryTask: resolve, stopTask: resolve, dismissTask: resolve };

function mount(session: ChatSessionState) {
  const host = document.createElement('div');
  document.body.appendChild(host);
  const root = createRoot(host);
  act(() => {
    root.render(<ChatSessionProvider session={session} actions={actions} tasks={deriveChatTasks({})}><ChecklistChip /></ChatSessionProvider>);
  });
  return { host, unmount: () => { act(() => root.unmount()); host.remove(); } };
}

describe('ChecklistChip', () => {
  it('renders nothing without a checklist', () => {
    const m = mount({ sessionId: 's1', running: false, compacting: false, agentAvatars: {} });
    expect(m.host.textContent).toBe('');
    m.unmount();
  });

  // The count is the session's progress; the button opens the list itself.
  it('counts the done items and opens the list', () => {
    const items = [{ content: 'read it', status: 'completed' }, { content: 'fix it', status: 'in_progress' }, { content: 'test it', status: 'pending' }];
    const m = mount({ sessionId: 's1', running: true, compacting: false, agentAvatars: {}, checklist: { callId: 'c9', items, done: 1 } });
    const button = m.host.querySelector('button')!;
    expect(button.textContent).toBe('Checklist 1/3');
    expect(m.host.querySelector('[role=region]')).toBeNull();
    act(() => button.click());
    expect(m.host.querySelectorAll('.ToolCallCard-todo').length).toBe(3);
    expect(m.host.querySelector('.ToolCallCard-todo--in_progress')?.textContent).toBe('fix it');
    act(() => button.click());
    expect(m.host.querySelector('[role=region]')).toBeNull();
    m.unmount();
  });
});
