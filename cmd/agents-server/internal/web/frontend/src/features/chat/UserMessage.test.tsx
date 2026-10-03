// @vitest-environment jsdom
import { describe, it, expect, beforeAll, afterAll, vi } from 'vitest';
import { act, type ReactNode, type Ref, type TextareaHTMLAttributes } from 'react';
import { createRoot } from 'react-dom/client';

vi.mock('@primer/react', () => ({
  Button: ({ children, onClick, disabled }: { children?: ReactNode; onClick?: () => void; disabled?: boolean }) => <button type="button" onClick={onClick} disabled={disabled}>{children}</button>,
  IconButton: ({ 'aria-label': label, onClick, disabled }: { 'aria-label'?: string; onClick?: () => void; disabled?: boolean }) => <button type="button" aria-label={label} onClick={onClick} disabled={disabled} />,
  Textarea: ({ block: _b, resize: _r, ...rest }: { block?: boolean; resize?: string } & TextareaHTMLAttributes<HTMLTextAreaElement> & { ref?: Ref<HTMLTextAreaElement> }) => <textarea {...rest} />,
}));
vi.mock('@primer/octicons-react', () => Object.fromEntries(
  ['PulseIcon', 'CopyIcon', 'CheckIcon', 'PencilIcon', 'ChevronLeftIcon', 'ChevronRightIcon'].map(n => [n, () => null]),
));
vi.mock('@/lib/hooks', () => ({ useCopy: () => ({ copied: false, copy: () => {} }) }));
vi.mock('@/features/chat/ZoomOverlay', () => ({ ZoomOverlay: () => null }));
import { ChatSessionProvider, type ChatActions, type ChatSessionState } from '@/features/chat/ChatSessionContext';
import { UserMessage } from '@/features/chat/UserMessage';

const g = globalThis as Record<string, unknown>;
let savedActEnv: unknown;
beforeAll(() => { savedActEnv = g.IS_REACT_ACT_ENVIRONMENT; g.IS_REACT_ACT_ENVIRONMENT = true; });
afterAll(() => { if (savedActEnv === undefined) delete g.IS_REACT_ACT_ENVIRONMENT; else g.IS_REACT_ACT_ENVIRONMENT = savedActEnv; });

function mount(props: Partial<Parameters<typeof UserMessage>[0]>, session: Partial<ChatSessionState> = {}) {
  const editResend = vi.fn();
  const switchBranch = vi.fn();
  const actions: ChatActions = { editResend, switchBranch, openTrace: () => {}, inspectTask: () => {}, retryTask: async () => {}, stopTask: async () => {}, dismissTask: async () => {} };
  const host = document.createElement('div');
  document.body.appendChild(host);
  const root = createRoot(host);
  act(() => {
    root.render(
      <ChatSessionProvider session={{ sessionId: 's1', running: false, compacting: false, agentAvatars: {}, ...session }} actions={actions} tasks={{ items: [], lookups: { retryableByCallId: {}, liveTaskStatusByCallId: {}, liveTaskLabelByCallId: {}, taskLabelById: {} } }}>
        <UserMessage content="do X" msgIdx={0} {...props} />
      </ChatSessionProvider>,
    );
  });
  const button = (label: string) => host.querySelector(`button[aria-label^="${label}"]`) as HTMLButtonElement | null;
  return { host, editResend, switchBranch, button, unmount: () => { act(() => root.unmount()); host.remove(); } };
}

describe('UserMessage edit', () => {
  // Edit opens the box and touches nothing; only Send branches and runs.
  it('branches on send, not on Edit', () => {
    const m = mount({ entryId: 'e5', parentId: 'e2' });
    act(() => m.button('Edit')!.click());
    expect(m.editResend).not.toHaveBeenCalled();
    const box = m.host.querySelector('textarea') as HTMLTextAreaElement;
    expect(box.value).toBe('do X');
    act(() => {
      const setter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')!.set!;
      setter.call(box, 'do Y instead');
      box.dispatchEvent(new Event('input', { bubbles: true }));
    });
    act(() => { box.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true })); });
    expect(m.editResend).toHaveBeenCalledWith('e5', 'e2', 'do Y instead', undefined);
    expect(m.host.querySelector('textarea')).toBeNull();
    m.unmount();
  });

  it('offers no Edit on the first message, while running, or while a decision is pending', () => {
    const first = mount({ entryId: 'e1' });
    expect(first.button('Edit')).toBeNull();
    first.unmount();
    const running = mount({ entryId: 'e5', parentId: 'e2' }, { running: true });
    expect(running.button('Edit')).toBeNull();
    running.unmount();
    const pending = mount({ entryId: 'e5', parentId: 'e2' }, { pendingDecision: true });
    expect(pending.button('Edit')).toBeNull();
    pending.unmount();
  });

  it('switches between the attempts of an edited message', () => {
    const m = mount({ entryId: 'e5', parentId: 'e2', branches: { parentId: 'e2', tips: ['e4', 'e6'], active: 1 } });
    expect(m.host.querySelector('.branch-count')?.textContent).toBe('2 / 2');
    act(() => m.button('Previous attempt')!.click());
    expect(m.switchBranch).toHaveBeenCalledWith('e4');
    m.unmount();
  });
});
