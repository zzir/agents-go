// @vitest-environment jsdom
import { describe, it, expect, beforeAll, afterAll, beforeEach, vi } from 'vitest';
import { act, type ReactNode } from 'react';
import { createRoot } from 'react-dom/client';

// Primer ships CSS the node loader cannot import; the composer's pieces are
// plain elements here. A menu renders its items inline, so a test can click
// them; the slash popup's rows keep their ids and roles.
vi.mock('@primer/react', () => {
  const Item = ({ children, id, role, onSelect, disabled }: { children?: ReactNode; id?: string; role?: string; onSelect?: () => void; disabled?: boolean }) => (
    <li id={id} role={role} aria-disabled={disabled || undefined} onClick={disabled ? undefined : onSelect}>{children}</li>
  );
  const ActionList = ({ children }: { children?: ReactNode }) => <ul>{children}</ul>;
  ActionList.Item = Item;
  ActionList.LeadingVisual = ({ children }: { children?: ReactNode }) => <span>{children}</span>;
  ActionList.Description = ({ children }: { children?: ReactNode }) => <span>{children}</span>;
  ActionList.Divider = () => <hr />;
  const ActionMenu = ({ children }: { children?: ReactNode }) => <div>{children}</div>;
  ActionMenu.Anchor = ({ children }: { children?: ReactNode }) => <>{children}</>;
  ActionMenu.Overlay = ({ children }: { children?: ReactNode }) => <div>{children}</div>;
  return {
    ActionList, ActionMenu,
    IconButton: ({ 'aria-label': label, onClick, disabled, type }: { 'aria-label'?: string; onClick?: (e: unknown) => void; disabled?: boolean; type?: 'submit' | 'button' }) => (
      <button type={type || 'button'} aria-label={label} onClick={onClick} disabled={disabled} />
    ),
    Spinner: () => null,
  };
});
vi.mock('@primer/octicons-react', () => Object.fromEntries(
  ['ImageIcon', 'PaperAirplaneIcon', 'PlusIcon', 'SquareCircleIcon', 'TriangleDownIcon', 'XIcon', 'SyncIcon', 'ChecklistIcon', 'WorkflowIcon']
    .map(n => [n, () => null]),
));
vi.mock('@/lib/api', () => ({ api: { attachments: { remove: async () => null }, workflows: { list: async () => [] } } }));
const toastMock = vi.hoisted(() => ({ info: vi.fn(), error: vi.fn() }));
vi.mock('@/lib/toast', () => ({ toast: toastMock }));
vi.mock('@/lib/hooks', () => ({ useApi: () => ({ data: null, loading: false, error: null, reload: () => {}, mutateData: () => {} }) }));
vi.mock('@/lib/attachments', () => ({
  fetchAttachmentConfig: async () => ({ enabled: false, max_count: 0 }),
  imageAffordance: () => ({ enabled: false, hint: '' }),
  isImageFile: () => false,
  uploadAttachment: async () => { throw new Error('no'); },
}));
import { MessageInput } from '@/features/chat/MessageInput';

const g = globalThis as Record<string, unknown>;
let savedActEnv: unknown;
beforeAll(() => { savedActEnv = g.IS_REACT_ACT_ENVIRONMENT; g.IS_REACT_ACT_ENVIRONMENT = true; });
afterAll(() => { if (savedActEnv === undefined) delete g.IS_REACT_ACT_ENVIRONMENT; else g.IS_REACT_ACT_ENVIRONMENT = savedActEnv; });
beforeEach(() => { localStorage.clear(); toastMock.info.mockClear(); toastMock.error.mockClear(); });

const valueSetter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')!.set!;

function mount(props: Partial<Parameters<typeof MessageInput>[0]> = {}) {
  const host = document.createElement('div');
  document.body.appendChild(host);
  const root = createRoot(host);
  const onSend = vi.fn();
  const onCancel = vi.fn();
  act(() => {
    root.render(<MessageInput sessionId="s1" onSend={onSend} onCancel={onCancel} disabled={false} running={false} {...props} />);
  });
  const textarea = () => host.querySelector('textarea') as HTMLTextAreaElement;
  // Typing as React sees it: the native setter, then an input event.
  const type = (text: string) => act(() => {
    valueSetter.call(textarea(), text);
    textarea().dispatchEvent(new Event('input', { bubbles: true }));
  });
  const key = (k: string, init: KeyboardEventInit = {}) => act(() => {
    textarea().dispatchEvent(new KeyboardEvent('keydown', { key: k, bubbles: true, cancelable: true, ...init }));
  });
  return { host, onSend, onCancel, textarea, type, key, unmount: () => { act(() => root.unmount()); host.remove(); } };
}

describe('MessageInput keys', () => {
  it('sends on Enter and clears the box; Shift+Enter keeps typing', () => {
    const m = mount();
    m.type('hello');
    m.key('Enter', { shiftKey: true });
    expect(m.onSend).not.toHaveBeenCalled();
    m.key('Enter');
    expect(m.onSend).toHaveBeenCalledWith('hello', undefined);
    expect(m.textarea().value).toBe('');
    m.unmount();
  });

  // An IME's Enter commits the candidate, never the message.
  it('ignores Enter while a composition is active', () => {
    const m = mount();
    m.type('你好');
    m.key('Enter', { isComposing: true });
    expect(m.onSend).not.toHaveBeenCalled();
    m.unmount();
  });

  it('walks the slash offer with the arrows, takes a row with Enter, and dismisses it with Escape', () => {
    const m = mount();
    m.type('/');
    const ta = m.textarea();
    expect(ta.getAttribute('aria-controls')).toBe('slash-commands');
    expect(ta.getAttribute('aria-activedescendant')).toBe('slash-command-0');
    m.key('ArrowDown');
    expect(ta.getAttribute('aria-activedescendant')).toBe('slash-command-1');
    m.key('ArrowUp');
    expect(ta.getAttribute('aria-activedescendant')).toBe('slash-command-0');
    m.key('Enter');
    expect(m.onSend).not.toHaveBeenCalled();
    expect(ta.value).toBe('/plan ');
    // A space ends the command, so the offer closes on its own.
    expect(ta.getAttribute('aria-controls')).toBeNull();
    m.type('/pl');
    expect(ta.getAttribute('aria-controls')).toBe('slash-commands');
    m.key('Escape');
    expect(ta.getAttribute('aria-controls')).toBeNull();
    expect(ta.value).toBe('/pl');
    m.unmount();
  });

  it('is disabled with the reason as its placeholder when blocked', () => {
    const m = mount({ blocked: 'Add an agent first' });
    expect(m.textarea().disabled).toBe(true);
    expect(m.textarea().placeholder).toBe('Add an agent first');
    m.unmount();
  });

  // A hint is something to know before typing: it stands in for the idle
  // placeholder, and gives way to the reason nothing can be sent at all.
  it('shows a hint as its placeholder, and the blocked reason over it', () => {
    const hinted = mount({ hint: 'Sending skips the pending call' });
    expect(hinted.textarea().placeholder).toBe('Sending skips the pending call');
    expect(hinted.textarea().disabled).toBe(false);
    hinted.unmount();
    const both = mount({ hint: 'Sending skips the pending call', blocked: 'Create an agent first' });
    expect(both.textarea().placeholder).toBe('Create an agent first');
    both.unmount();
  });

  it('offers the graceful stop in a menu while running', () => {
    const m = mount({ running: true });
    expect(m.host.querySelector('button[aria-label="More actions for this run"]')).not.toBeNull();
    // Without a way to queue there is nothing to send while it runs.
    expect(m.host.querySelector('button[aria-label="Send now"]')).toBeNull();
    const items = [...m.host.querySelectorAll('li')];
    expect(items.map(li => li.textContent)).toEqual([
      'Stop nowCancels the run where it is.',
      'Finish this turn, then stopThe current step completes; no further turn starts.',
    ]);
    act(() => items[1].click());
    expect(m.onCancel).toHaveBeenCalledWith(true);
    act(() => items[0].click());
    expect(m.onCancel).toHaveBeenCalledWith(false);
    m.unmount();
  });
});

describe('MessageInput while a run is live', () => {
  const live = (extra: Partial<Parameters<typeof MessageInput>[0]> = {}) => {
    const onQueue = vi.fn();
    return { onQueue, ...mount({ running: true, disabled: true, onQueue, ...extra }) };
  };

  // Enter used to do nothing while a run was going, without a word.
  it('queues what is typed as a steer on Enter, and does not send', () => {
    const m = live();
    expect(m.textarea().placeholder).toBe('Running — Enter queues your message');
    m.type('  use staging instead ');
    m.key('Enter');
    expect(m.onQueue.mock.calls).toEqual([['use staging instead', 'steer']]);
    expect(m.onSend).not.toHaveBeenCalled();
    expect(m.textarea().value).toBe('');
    // Nothing typed, nothing queued.
    m.key('Enter');
    expect(m.onQueue).toHaveBeenCalledTimes(1);
    m.unmount();
  });

  it('Send now steers too, and is off while the box is empty', () => {
    const m = live();
    const send = () => m.host.querySelector('button[aria-label="Send now"]') as HTMLButtonElement;
    expect(send().disabled).toBe(true);
    m.type('check the logs first');
    expect(send().disabled).toBe(false);
    act(() => send().click());
    expect(m.onQueue.mock.calls).toEqual([['check the logs first', 'steer']]);
    m.unmount();
  });

  it('offers Send after this run in the run menu, queued as a follow-up', () => {
    const m = live();
    const item = () => [...m.host.querySelectorAll('li')].find(li => li.textContent?.startsWith('Send after this run'))!;
    expect(item().getAttribute('aria-disabled')).toBe('true');
    m.type('then write the changelog');
    expect(item().getAttribute('aria-disabled')).toBeNull();
    act(() => item().click());
    expect(m.onQueue.mock.calls).toEqual([['then write the changelog', 'follow_up']]);
    expect(m.textarea().value).toBe('');
    m.unmount();
  });

  // What sets up a new run cannot ride on one that is already going: the
  // text stays for when it finishes, and the box says why.
  it('keeps /plan and /workflow for a new run', () => {
    const m = live();
    for (const text of ['/plan rewrite the parser', '/workflow release 1.2']) {
      m.type(text);
      m.key('Enter');
      expect(m.textarea().value).toBe(text);
    }
    expect(m.onQueue).not.toHaveBeenCalled();
    expect(toastMock.info).toHaveBeenCalledTimes(2);
    m.unmount();
  });
});
