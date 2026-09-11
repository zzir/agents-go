// @vitest-environment jsdom
import { afterAll, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, type ReactNode } from 'react';
import { createRoot } from 'react-dom/client';

// Primer ships CSS the node loader cannot import. The Dialog stub exposes its
// two close gestures as buttons; the confirm answers what the test set.
const primer = vi.hoisted(() => ({ answer: true, confirm: vi.fn(async () => primer.answer) }));
vi.mock('@primer/react', () => {
  const NavList = ({ children }: { children: ReactNode }) => <ul>{children}</ul>;
  NavList.Item = ({ children, onClick }: { children: ReactNode; onClick: () => void }) => <li onClick={onClick}>{children}</li>;
  NavList.LeadingVisual = ({ children }: { children: ReactNode }) => <span>{children}</span>;
  NavList.Divider = () => <hr />;
  const Dialog = ({ children, onClose }: { children: ReactNode; onClose: (g: string) => void }) => (
    <div>
      <button type="button" data-gesture="escape" onClick={() => onClose('escape')}>esc</button>
      <button type="button" data-gesture="close-button" onClick={() => onClose('close-button')}>x</button>
      {children}
    </div>
  );
  Dialog.Body = ({ children }: { children: ReactNode }) => <div>{children}</div>;
  return {
    Dialog, NavList,
    Flash: ({ children }: { children: ReactNode }) => <div>{children}</div>,
    useConfirm: () => primer.confirm,
  };
});
import { UnsavedForm } from '@/components/UnsavedForm';
import { PanelDialog } from './PanelDialog';

const g = globalThis as Record<string, unknown>;
let savedActEnv: unknown;
beforeAll(() => {
  savedActEnv = g.IS_REACT_ACT_ENVIRONMENT; g.IS_REACT_ACT_ENVIRONMENT = true;
  window.matchMedia = (() => ({ matches: false, addEventListener: () => undefined, removeEventListener: () => undefined })) as unknown as typeof window.matchMedia;
});
afterAll(() => { if (savedActEnv === undefined) delete g.IS_REACT_ACT_ENVIRONMENT; else g.IS_REACT_ACT_ENVIRONMENT = savedActEnv; });
beforeEach(() => { primer.confirm.mockClear(); primer.answer = true; });

function Panel() { return <UnsavedForm><input aria-label="name" /></UnsavedForm>; }
const Icon = () => <svg />;
const tabs = [{ key: 'a', label: 'A', icon: Icon as never, load: async () => ({ default: Panel }) }];

async function open() {
  const onClose = vi.fn();
  const el = document.createElement('div');
  document.body.appendChild(el);
  const root = createRoot(el);
  await act(async () => root.render(<PanelDialog title="Settings" tabs={tabs} readOnly={false} onClose={onClose} />));
  // The tab's chunk resolves after a microtask.
  await act(async () => { await Promise.resolve(); });
  const gesture = (name: string) => act(async () => { (el.querySelector(`[data-gesture="${name}"]`) as HTMLButtonElement).click(); });
  const edit = () => act(async () => { el.querySelector('input')!.dispatchEvent(new Event('input', { bubbles: true })); });
  return { onClose, gesture, edit, unmount: () => act(async () => { root.unmount(); el.remove(); }) };
}

describe('PanelDialog unsaved guard', () => {
  it('closes a dialog without edits at once', async () => {
    const d = await open();
    await d.gesture('escape');
    expect(primer.confirm).not.toHaveBeenCalled();
    expect(d.onClose).toHaveBeenCalledTimes(1);
    await d.unmount();
  });

  it('asks before discarding an edited form on Escape and on the close button', async () => {
    const d = await open();
    await d.edit();
    primer.answer = false;
    await d.gesture('escape');
    await d.gesture('close-button');
    expect(primer.confirm).toHaveBeenCalledTimes(2);
    expect(d.onClose).not.toHaveBeenCalled();
    primer.answer = true;
    await d.gesture('escape');
    expect(d.onClose).toHaveBeenCalledTimes(1);
    await d.unmount();
  });
});
