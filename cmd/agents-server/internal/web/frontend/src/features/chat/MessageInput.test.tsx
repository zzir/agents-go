// @vitest-environment jsdom
import { describe, it, expect, beforeAll, afterAll, beforeEach, vi } from 'vitest';
import { act, type ReactNode } from 'react';
import { createRoot } from 'react-dom/client';

// Primer ships CSS the node loader cannot import; the composer's pieces are
// plain elements here. A menu renders its items inline, so a test can click
// them; the slash popup's rows keep their ids and roles.
vi.mock('@primer/react', () => {
  const Item = ({ children, id, role, onSelect }: { children?: ReactNode; id?: string; role?: string; onSelect?: () => void }) => (
    <li id={id} role={role} onClick={onSelect}>{children}</li>
  );
  const ActionList = ({ children }: { children?: ReactNode }) => <ul>{children}</ul>;
  ActionList.Item = Item;
  ActionList.LeadingVisual = ({ children }: { children?: ReactNode }) => <span>{children}</span>;
  ActionList.Description = ({ children }: { children?: ReactNode }) => <span>{children}</span>;
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
vi.mock('@/lib/toast', () => ({ toast: { info: () => {}, error: () => {} } }));
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
beforeEach(() => { localStorage.clear(); });

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

  it('offers the graceful stop in a menu while running', () => {
    const m = mount({ running: true });
    expect(m.host.querySelector('button[aria-label="More ways to stop"]')).not.toBeNull();
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
