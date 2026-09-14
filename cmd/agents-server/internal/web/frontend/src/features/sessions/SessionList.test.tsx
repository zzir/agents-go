// @vitest-environment jsdom
import { describe, it, expect, beforeAll, afterAll, vi } from 'vitest';
import { act, useEffect, useState, type ReactNode } from 'react';
import { createRoot } from 'react-dom/client';

// Primer ships CSS the node loader cannot import; the list's pieces are plain
// elements here — a row is an <li> holding exactly what the row renders.
vi.mock('@primer/react', () => {
  const Item = ({ children, className }: { children?: ReactNode; className?: string }) => <li className={className}>{children}</li>;
  const ActionList = ({ children }: { children?: ReactNode }) => <ul>{children}</ul>;
  ActionList.Item = Item;
  ActionList.Group = ({ children }: { children?: ReactNode }) => <div>{children}</div>;
  ActionList.GroupHeading = ({ children }: { children?: ReactNode }) => <h3>{children}</h3>;
  ActionList.TrailingAction = ({ label }: { label?: string }) => <button type="button" aria-label={label} />;
  ActionList.LeadingVisual = ({ children }: { children?: ReactNode }) => <span>{children}</span>;
  ActionList.Divider = () => <hr />;
  const ActionMenu = ({ children }: { children?: ReactNode }) => <>{children}</>;
  ActionMenu.Overlay = () => null;
  const TextInput = () => <input />;
  TextInput.Action = () => null;
  return {
    ActionList, ActionMenu, TextInput,
    Dialog: () => null,
    FormControl: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
    IconButton: ({ 'aria-label': label }: { 'aria-label'?: string }) => <button type="button" aria-label={label} />,
    useConfirm: () => async () => false,
  };
});
vi.mock('@primer/octicons-react', () => Object.fromEntries(
  ['KebabHorizontalIcon', 'PencilIcon', 'PinIcon', 'PinSlashIcon', 'PlusIcon', 'RepoForkedIcon', 'SearchIcon', 'TrashIcon', 'WorkflowIcon', 'XIcon']
    .map(n => [n, () => null]),
));
const { rows } = vi.hoisted(() => ({ rows: { value: [] as { id: string; name: string; pinned: boolean }[] } }));
vi.mock('@/lib/api', () => ({ api: { sessions: { list: async () => rows.value } } }));
vi.mock('@/lib/toast', () => ({ toast: { error: () => {} } }));
vi.mock('@/lib/hooks', () => ({
  useApi: (fetcher: () => Promise<unknown>) => {
    const [data, setData] = useState<unknown>(null);
    useEffect(() => {
      let alive = true;
      fetcher().then(d => { if (alive) setData(d); });
      return () => { alive = false; };
      // eslint-disable-next-line react-hooks/exhaustive-deps
    }, []);
    return { data, loading: data === null, error: null, reload: () => {}, mutateData: () => {} };
  },
}));
import { SessionList } from '@/features/sessions/SessionList';

const g = globalThis as Record<string, unknown>;
let savedActEnv: unknown;
beforeAll(() => { savedActEnv = g.IS_REACT_ACT_ENVIRONMENT; g.IS_REACT_ACT_ENVIRONMENT = true; });
afterAll(() => { if (savedActEnv === undefined) delete g.IS_REACT_ACT_ENVIRONMENT; else g.IS_REACT_ACT_ENVIRONMENT = savedActEnv; });

const noop = () => {};

async function mount(props: Partial<Parameters<typeof SessionList>[0]> = {}) {
  const host = document.createElement('div');
  document.body.appendChild(host);
  const root = createRoot(host);
  await act(async () => {
    root.render(<SessionList activeId={null} onSelect={noop} onNew={noop} onOpenHub={noop} reloadKey={0} {...props} />);
  });
  await act(async () => {});
  return { host, unmount: () => { act(() => root.unmount()); host.remove(); } };
}

describe('SessionList', () => {
  it('calls an empty list what it is', async () => {
    rows.value = [];
    const { host, unmount } = await mount();
    expect(host.querySelector('.blankslate')?.textContent).toBe('No sessions yet');
    unmount();
  });

  // The running and awaiting bars are color alone; the row's text carries the
  // words for a screen reader.
  it('says in words which sessions run and which wait for approval', async () => {
    rows.value = [{ id: 'a', name: 'Alpha', pinned: false }, { id: 'b', name: 'Beta', pinned: false }];
    const { host, unmount } = await mount({ runningSessions: new Set(['a']), awaitingSessions: new Set(['b']) });
    const text = (name: string) => [...host.querySelectorAll('li')].find(li => li.textContent?.startsWith(name))?.textContent;
    expect(text('Alpha')).toBe('Alpha — running');
    expect(text('Beta')).toBe('Beta — awaiting your approval');
    expect(host.querySelector('li .sr-only')).not.toBeNull();
    unmount();
  });
});
