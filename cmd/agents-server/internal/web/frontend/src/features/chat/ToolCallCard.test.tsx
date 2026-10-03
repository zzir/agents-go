// @vitest-environment jsdom
import { describe, it, expect, beforeAll, afterAll, vi } from 'vitest';
import { act, type ReactNode, type Ref, type TextareaHTMLAttributes } from 'react';
import { createRoot } from 'react-dom/client';

// Primer ships CSS the node loader cannot import; the card's pieces are plain
// elements here — a menu renders its items inline, so a test can click them —
// and the markdown pipeline (a worker) is not what this test is about.
vi.mock('@primer/react', () => {
  const ActionList = ({ children }: { children?: ReactNode }) => <ul>{children}</ul>;
  ActionList.Item = ({ children, onSelect }: { children?: ReactNode; onSelect?: () => void }) => <li><button type="button" onClick={onSelect}>{children}</button></li>;
  const ActionMenu = ({ children }: { children?: ReactNode }) => <>{children}</>;
  ActionMenu.Anchor = ({ children }: { children?: ReactNode }) => <>{children}</>;
  ActionMenu.Overlay = ({ children }: { children?: ReactNode }) => <>{children}</>;
  return {
    ActionList, ActionMenu,
    Button: ({ children, onClick }: { children?: ReactNode; onClick?: () => void }) => <button type="button" onClick={onClick}>{children}</button>,
    ButtonGroup: ({ children }: { children?: ReactNode }) => <>{children}</>,
    IconButton: ({ 'aria-label': label }: { 'aria-label'?: string }) => <button type="button" aria-label={label} />,
    Label: ({ children }: { children?: ReactNode }) => <span>{children}</span>,
    Textarea: ({ block: _block, resize: _resize, ...rest }: { block?: boolean; resize?: string } & TextareaHTMLAttributes<HTMLTextAreaElement> & { ref?: Ref<HTMLTextAreaElement> }) => <textarea {...rest} />,
  };
});
vi.mock('@primer/octicons-react', () => Object.fromEntries(
  ['ToolsIcon', 'StackIcon', 'SyncIcon', 'CheckIcon', 'DotFillIcon', 'CircleIcon', 'ChevronRightIcon', 'TriangleDownIcon'].map(n => [n, () => null]),
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
  // The decision buttons by their words; the reject menu's unlabelled caret is left out.
  const buttons = () => ([...host.querySelectorAll('.ToolCallCard-approval button')] as HTMLButtonElement[]).filter(b => b.textContent);
  const click = (label: string) => act(() => buttons().find(b => b.textContent === label)!.click());
  return { host, approve, reject, buttons, click, unmount: () => { act(() => root.unmount()); host.remove(); } };
}

const pending = (tool_name: string, args: Record<string, unknown>): ToolCall => ({
  tool_call_id: 'c1', tool_name, arguments: JSON.stringify(args), needs_approval: true,
} as ToolCall);

describe('ToolCallCard approval', () => {
  it('offers exec_command its three trust tiers and a reject, each sending its scope', () => {
    const m = mount(pending('exec_command', { cmd: 'ls -la' }));
    expect(m.buttons().map(b => b.textContent)).toEqual(['Approve once', 'Trust this command', 'Trust all this session', 'Reject', 'Reject with reason…']);
    m.click('Approve once');
    m.click('Trust this command');
    m.click('Trust all this session');
    expect(m.approve.mock.calls).toEqual([['c1', 'once'], ['c1', 'same'], ['c1', 'all']]);
    m.click('Reject');
    expect(m.reject).toHaveBeenCalledWith('c1', undefined);
    m.unmount();
  });

  // The reason is what the model reads as the rejected call's output: typed
  // in the card, sent with the rejection.
  it('Reject with reason sends the typed reason', () => {
    const m = mount(pending('exec_command', { cmd: 'make deploy' }));
    expect(m.host.querySelector('textarea')).toBeNull();
    m.click('Reject with reason…');
    const box = m.host.querySelector('textarea') as HTMLTextAreaElement;
    const key = (k: string, init: KeyboardEventInit = {}) => act(() => {
      box.dispatchEvent(new KeyboardEvent('keydown', { key: k, bubbles: true, cancelable: true, ...init }));
    });
    act(() => {
      Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')!.set!.call(box, '  use staging ');
      box.dispatchEvent(new Event('input', { bubbles: true }));
    });
    // An IME's Enter confirms the composition — Safari marks the one after
    // compositionend with keyCode 229 alone — and its Escape cancels the
    // composition, not the box; Shift+Enter is a new line.
    key('Enter', { isComposing: true });
    key('Enter', { keyCode: 229 });
    key('Escape', { isComposing: true });
    key('Enter', { shiftKey: true });
    expect(m.reject).not.toHaveBeenCalled();
    expect(m.host.querySelector('textarea')).not.toBeNull();
    key('Enter');
    expect(m.reject.mock.calls).toEqual([['c1', 'use staging']]);
    m.unmount();
  });

  // A reason typed and then the Reject button clicked: the reason goes with it.
  it('Reject sends the reason the open box holds', () => {
    const m = mount(pending('exec_command', { cmd: 'make deploy' }));
    m.click('Reject with reason…');
    const box = m.host.querySelector('textarea') as HTMLTextAreaElement;
    act(() => {
      Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')!.set!.call(box, 'wrong target');
      box.dispatchEvent(new Event('input', { bubbles: true }));
    });
    m.click('Reject');
    expect(m.reject.mock.calls).toEqual([['c1', 'wrong target']]);
    m.unmount();
  });

  it('Escape closes the reason box without rejecting, and an empty reason is a plain reject', () => {
    const m = mount(pending('exec_command', { cmd: 'ls' }));
    m.click('Reject with reason…');
    const key = (k: string) => act(() => {
      m.host.querySelector('textarea')!.dispatchEvent(new KeyboardEvent('keydown', { key: k, bubbles: true, cancelable: true }));
    });
    key('Escape');
    expect(m.host.querySelector('textarea')).toBeNull();
    expect(m.reject).not.toHaveBeenCalled();
    m.click('Reject with reason…');
    key('Enter');
    expect(m.reject.mock.calls).toEqual([['c1', undefined]]);
    m.unmount();
  });

  it('offers any other tool one approve, named for what it does', () => {
    const a = mount(pending('memory_write', { key: 'k', text: 't' }));
    expect(a.buttons().map(b => b.textContent)).toEqual(['Approve', 'Reject', 'Reject with reason…']);
    a.click('Approve');
    expect(a.approve).toHaveBeenCalledWith('c1', 'once');
    a.unmount();
    // A plan's rejection is feedback to revise from: the card asks for it in
    // those words, and sends it as the reason.
    const b = mount(pending('submit_plan', { plan: '# plan' }));
    expect(b.buttons().map(x => x.textContent)).toEqual(['Approve plan', 'Reject', 'Keep planning…']);
    b.click('Keep planning…');
    const box = b.host.querySelector('textarea') as HTMLTextAreaElement;
    expect(box.placeholder).toContain('The model revises the plan');
    act(() => {
      Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')!.set!.call(box, 'split step 2');
      box.dispatchEvent(new Event('input', { bubbles: true }));
    });
    act(() => { box.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true })); });
    expect(b.reject.mock.calls).toEqual([['c1', 'split step 2']]);
    b.unmount();
    const c = mount(pending('save_workflow', { name: 'w' }));
    expect(c.buttons()[0].textContent).toBe('Save workflow');
    c.unmount();
  });

  // With the card focused, y approves (once, for a command) and n opens the
  // reason box; a key typed into the box itself is text, not a decision.
  it('takes y and n from the keyboard while the card has focus', () => {
    const m = mount(pending('exec_command', { cmd: 'ls' }));
    const header = m.host.querySelector('.disclosure-header') as HTMLElement;
    const key = (el: Element, key: string) => act(() => { el.dispatchEvent(new KeyboardEvent('keydown', { key, bubbles: true })); });
    header.focus();
    key(header, 'n');
    const box = m.host.querySelector('textarea');
    expect(box).not.toBeNull();
    key(box!, 'y');
    key(box!, 'n');
    expect(m.approve).not.toHaveBeenCalled();
    key(header, 'y');
    expect(m.approve).toHaveBeenCalledWith('c1', 'once');
    expect(document.activeElement).toBe(header);
    m.unmount();
    const other = mount(pending('write_file', { path: 'a' }));
    key(other.host.querySelector('.disclosure-header')!, 'y');
    expect(other.approve).toHaveBeenCalledWith('c1', 'once');
    other.unmount();
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
