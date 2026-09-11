// @vitest-environment jsdom
import { describe, it, expect, beforeAll, afterAll, vi } from 'vitest';
import { act, type ReactNode } from 'react';
import { createRoot } from 'react-dom/client';

// Primer ships CSS the node loader cannot import; the card's Button and Label
// are plain elements here, and the markdown pipeline (a worker) is not what
// this test is about.
vi.mock('@primer/react', () => ({
  Button: ({ children, onClick }: { children?: ReactNode; onClick?: () => void }) => <button type="button" onClick={onClick}>{children}</button>,
  Label: ({ children }: { children?: ReactNode }) => <span>{children}</span>,
}));
vi.mock('@primer/octicons-react', () => Object.fromEntries(
  ['ToolsIcon', 'StackIcon', 'SyncIcon', 'CheckIcon', 'DotFillIcon', 'CircleIcon', 'ChevronRightIcon'].map(n => [n, () => null]),
));
vi.mock('@/lib/markdown', () => ({ useAsyncMarkdown: () => '' }));
vi.mock('@/features/chat/ToolOutputBody', () => ({ ToolOutputBody: () => null }));
vi.mock('@/features/chat/WorkflowSpecBody', () => ({ WorkflowSpecBody: () => null }));
import { ChatSessionProvider, deriveChatTasks, type ChatActions, type ChatSessionState } from '@/features/chat/ChatSessionContext';
import { ToolCallCard } from '@/features/chat/ToolCallCard';
import type { ToolCall } from '@/lib/timeline';

const g = globalThis as Record<string, unknown>;
let savedActEnv: unknown;
beforeAll(() => { savedActEnv = g.IS_REACT_ACT_ENVIRONMENT; g.IS_REACT_ACT_ENVIRONMENT = true; });
afterAll(() => { if (savedActEnv === undefined) delete g.IS_REACT_ACT_ENVIRONMENT; else g.IS_REACT_ACT_ENVIRONMENT = savedActEnv; });

const SESSION: ChatSessionState = { sessionId: 's1', running: true, compacting: false, agentAvatars: {} };
const noop = () => {};
const resolve = async () => {};

function mount(toolCall: ToolCall) {
  const approve = vi.fn();
  const reject = vi.fn();
  const actions: ChatActions = { approve, reject, openTrace: noop, inspectTask: noop, retryTask: resolve, stopTask: resolve, dismissTask: resolve };
  const host = document.createElement('div');
  document.body.appendChild(host);
  const root = createRoot(host);
  act(() => {
    root.render(
      <ChatSessionProvider session={SESSION} actions={actions} tasks={deriveChatTasks({})}>
        <ToolCallCard toolCall={toolCall} live />
      </ChatSessionProvider>,
    );
  });
  const buttons = () => [...host.querySelectorAll('.ToolCallCard-approval button')] as HTMLButtonElement[];
  const click = (label: string) => act(() => buttons().find(b => b.textContent === label)!.click());
  return { host, approve, reject, buttons, click, unmount: () => { act(() => root.unmount()); host.remove(); } };
}

const pending = (tool_name: string, args: Record<string, unknown>): ToolCall => ({
  tool_call_id: 'c1', tool_name, arguments: JSON.stringify(args), needs_approval: true,
} as ToolCall);

describe('ToolCallCard approval', () => {
  it('offers exec_command its three trust tiers and a reject, each sending its scope', () => {
    const m = mount(pending('exec_command', { cmd: 'ls -la' }));
    expect(m.buttons().map(b => b.textContent)).toEqual(['Approve once', 'Trust this command', 'Trust all this session', 'Reject']);
    m.click('Approve once');
    m.click('Trust this command');
    m.click('Trust all this session');
    expect(m.approve.mock.calls).toEqual([['c1', 'once'], ['c1', 'same'], ['c1', 'all']]);
    m.click('Reject');
    expect(m.reject).toHaveBeenCalledWith('c1');
    m.unmount();
  });

  it('offers any other tool one approve, named for what it does', () => {
    const a = mount(pending('memory_write', { key: 'k', text: 't' }));
    expect(a.buttons().map(b => b.textContent)).toEqual(['Approve', 'Reject']);
    a.click('Approve');
    expect(a.approve).toHaveBeenCalledWith('c1', 'once');
    a.unmount();
    const b = mount(pending('submit_plan', { plan: '# plan' }));
    expect(b.buttons()[0].textContent).toBe('Approve plan');
    b.unmount();
    const c = mount(pending('save_workflow', { name: 'w' }));
    expect(c.buttons()[0].textContent).toBe('Save workflow');
    c.unmount();
  });

  // The decision unmounts the buttons; focus lands on the card's header, not
  // on <body>.
  it('moves focus to the card before a decision unmounts its buttons', () => {
    const m = mount(pending('exec_command', { cmd: 'ls' }));
    m.click('Reject');
    expect(document.activeElement).toBe(m.host.querySelector('.disclosure-header'));
    m.unmount();
  });

  // A call an abandoned pause never ran offers no decision and says why.
  it('a not-run call offers no decision and names the reason', () => {
    const m = mount({ ...pending('exec_command', { cmd: 'ls' }), status: 'not_run', not_run: 'superseded' });
    expect(m.buttons()).toEqual([]);
    expect(m.host.textContent).toContain('not run — superseded by a newer message');
    m.unmount();
    const s = mount({ ...pending('exec_command', { cmd: 'ls' }), status: 'not_run', not_run: 'stopped' });
    expect(s.host.textContent).toContain('not run — stopped');
    s.unmount();
  });

  it('shows the command as typed, with its working directory', () => {
    const m = mount(pending('exec_command', { cmd: 'make test', workdir: 'src' }));
    expect(m.host.querySelector('.disclosure-body pre')?.textContent).toBe('cd src && make test');
    m.unmount();
  });
});
