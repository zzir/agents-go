// @vitest-environment jsdom
import { describe, it, expect, beforeAll, afterAll, vi } from 'vitest';
import { act, type ReactNode } from 'react';
import { createRoot } from 'react-dom/client';

/* Primer ships CSS the node loader cannot import, and its ActionMenu renders
   through a portal. Both are replaced by the plain elements this test is
   actually about: what the menu offers, in what order. */
vi.mock('@primer/react', () => {
  const Item = ({ children, disabled, variant }: { children?: ReactNode; disabled?: boolean; variant?: string }) => (
    <li aria-disabled={disabled ? 'true' : undefined} data-variant={variant}>{children}</li>
  );
  Item.LeadingVisual = ({ children }: { children?: ReactNode }) => <span>{children}</span>;
  Item.Description = ({ children }: { children?: ReactNode }) => <span>{children}</span>;
  const ActionList = ({ children }: { children?: ReactNode }) => <ul>{children}</ul>;
  ActionList.Item = Item;
  ActionList.LeadingVisual = Item.LeadingVisual;
  ActionList.Description = Item.Description;
  ActionList.Divider = () => <hr />;
  const ActionMenu = ({ children }: { children?: ReactNode }) => <div>{children}</div>;
  ActionMenu.Anchor = ({ children }: { children?: ReactNode }) => <>{children}</>;
  ActionMenu.Overlay = ({ children }: { children?: ReactNode }) => <div>{children}</div>;
  return {
    ActionList,
    ActionMenu,
    IconButton: ({ 'aria-label': label, disabled }: { 'aria-label'?: string; disabled?: boolean }) => (
      <button aria-label={label} disabled={disabled} />
    ),
  };
});
vi.mock('@primer/octicons-react', () => ({ default: {}, ...Object.fromEntries(
  ['FileDirectoryIcon', 'KeyAsteriskIcon', 'KebabHorizontalIcon', 'BrowserIcon', 'DownloadIcon', 'MeterIcon', 'PlayIcon', 'PulseIcon', 'SquareFillIcon', 'StackIcon', 'SyncIcon', 'TerminalIcon']
    .map(n => [n, () => null]),
) }));
vi.mock('@/features/chat/ChatSessionContext', () => ({ useChatSession: () => ({ sessionId: 's1' }) }));

import { ChatTopBar } from '@/features/chat/ChatTopBar';

const g = globalThis as Record<string, unknown>;
let savedActEnv: unknown;
beforeAll(() => { savedActEnv = g.IS_REACT_ACT_ENVIRONMENT; g.IS_REACT_ACT_ENVIRONMENT = true; });
afterAll(() => { if (savedActEnv === undefined) delete g.IS_REACT_ACT_ENVIRONMENT; else g.IS_REACT_ACT_ENVIRONMENT = savedActEnv; });

const noop = () => {};

function render(props: Partial<Parameters<typeof ChatTopBar>[0]> = {}): HTMLElement {
  const host = document.createElement('div');
  document.body.appendChild(host);
  act(() => {
    createRoot(host).render(
      <ChatTopBar
        sessionName="s"
        panel={null}
        onPanelChange={noop}
        terminalEnabled
        onTerminalOpen={noop}
        binding={{ title: 'sb — proj', projectName: 'proj' }}
        projectMenu={{ busy: false, state: 'running', onEnv: noop, onStart: noop, onStop: noop, onExport: noop, onPreview: noop, onRebuild: noop }}
        {...props}
      />,
    );
  });
  return host;
}

describe('ChatTopBar', () => {
  it('offers the terminal, the environment, the compute switch and the rebuild, in that order', () => {
    const host = render();
    const items = [...host.querySelectorAll('li')].map(li => li.textContent);
    expect(items).toEqual(['Terminal panel', 'Environment…', 'Preview a port…Open a service running inside the sandbox.', 'Export as tar…The whole working tree, as a download.', 'Stop sandboxKeeps the files; frees the memory.', 'Rebuild container']);
  });

  // A running sandbox offers Stop; anything else offers Start, and says why.
  it('offers Start when the sandbox is not running', () => {
    const host = render({ projectMenu: { busy: false, state: 'absent', onEnv: noop, onStart: noop, onStop: noop, onExport: noop, onPreview: noop, onRebuild: noop } });
    const items = [...host.querySelectorAll('li')].map(li => li.textContent);
    expect(items[4]).toBe('Start sandboxNot created yet — pulls the image.');
  });

  it('marks only the rebuild as destructive', () => {
    const host = render();
    const variants = [...host.querySelectorAll('li')].map(li => li.getAttribute('data-variant'));
    expect(variants).toEqual([null, null, null, null, null, 'danger']);
  });

  /* The terminal left the top bar for the project menu: the three buttons
     there are inspector lenses, and what the terminal opens belongs to the
     project. A button that comes back would put it in both places. */
  it('keeps the top-bar buttons to the three inspector lenses', () => {
    const host = render();
    const labels = [...host.querySelectorAll('button[aria-label]')].map(b => b.getAttribute('aria-label'));
    expect(labels).toEqual(['Actions for proj', 'Tasks', 'Traces', 'Context']);
  });

  it('disables the terminal item when no sandbox can serve one', () => {
    const host = render({ terminalEnabled: false });
    expect(host.querySelector('li')?.getAttribute('aria-disabled')).toBe('true');
  });

  /* An unbound session has no project, so no menu — and therefore no way to
     the terminal until its first message binds it. */
  it('shows no menu while the session is unbound', () => {
    const host = render({ binding: null, projectMenu: null });
    expect(host.querySelectorAll('li')).toHaveLength(0);
  });
});
