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
const { rows, listCalls } = vi.hoisted(() => ({
  rows: { value: [] as { id: string; name: string; pinned: boolean; status?: string }[] },
  listCalls: [] as Array<{ limit?: number; q?: string }>,
}));
vi.mock('@/lib/api', () => ({ api: { sessions: { list: async (opts: { limit?: number; q?: string } = {}) => { listCalls.push(opts); return rows.value.slice(0, opts.limit ? opts.limit + rows.value.filter(r => r.pinned).length : undefined); } } } }));
vi.mock('@/lib/toast', () => ({ toast: { error: () => {} } }));
vi.mock('@/lib/hooks', () => ({
  // Re-fetches when the deps move, as the real hook does; no cache.
  useApi: (fetcher: () => Promise<unknown>, deps: unknown[] = []) => {
    const [data, setData] = useState<unknown>(null);
    useEffect(() => {
      let alive = true;
      fetcher().then(d => { if (alive) setData(d); });
      return () => { alive = false; };
      // eslint-disable-next-line react-hooks/exhaustive-deps
    }, deps);
    return { data, loading: data === null, error: null, reload: () => {}, mutateData: () => {} };
  },
  useDebouncedValue: <T,>(v: T) => v,
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
  // words for a screen reader. Both come from the status the server derived —
  // the list row's — with nothing of the conversation loaded.
  it('says in words which sessions run and which wait for approval', async () => {
    rows.value = [
      { id: 'a', name: 'Alpha', pinned: false, status: 'running' },
      { id: 'b', name: 'Beta', pinned: false, status: 'requires_action' },
      { id: 'c', name: 'Gamma', pinned: false, status: 'failed' },
      { id: 'd', name: 'Delta', pinned: false },
    ];
    const { host, unmount } = await mount();
    const text = (name: string) => [...host.querySelectorAll('li')].find(li => li.textContent?.startsWith(name))?.textContent;
    expect(text('Alpha')).toBe('Alpha — running');
    expect(text('Beta')).toBe('Beta — awaiting your approval');
    // A failed or idle conversation carries no marker.
    expect(text('Gamma')).toBe('Gamma');
    expect(text('Delta')).toBe('Delta');
    expect(host.querySelector('li .sr-only')).not.toBeNull();
    unmount();
  });

  // What session.status announced since the list was read wins over the row.
  it('shows the status last announced over the one the list carried', async () => {
    rows.value = [
      { id: 'a', name: 'Alpha', pinned: false, status: 'running' },
      { id: 'b', name: 'Beta', pinned: false, status: 'idle' },
    ];
    const { host, unmount } = await mount({ statuses: { a: 'idle', b: 'requires_action' } });
    const text = (name: string) => [...host.querySelectorAll('li')].find(li => li.textContent?.startsWith(name))?.textContent;
    expect(text('Alpha')).toBe('Alpha');
    expect(text('Beta')).toBe('Beta — awaiting your approval');
    unmount();
  });

  // The list is read a page at a time: a page that fills its limit ends in
  // "Show more", which asks for a longer prefix; a short page is the end.
  it('loads more only while a page is full, by asking for a longer prefix', async () => {
    rows.value = [{ id: 'p', name: 'Pinned', pinned: true }];
    for (let i = 0; i < 150; i++) rows.value.push({ id: 's' + i, name: 'Session ' + i, pinned: false });
    listCalls.length = 0;
    const { host, unmount } = await mount();
    expect(listCalls[0]).toEqual({ limit: 100, q: undefined });
    expect(host.querySelectorAll('li').length).toBe(101);
    const more = host.querySelector('.sidebar-more-button') as HTMLButtonElement | null;
    expect(more?.textContent).toBe('Show more');
    await act(async () => { more!.click(); });
    await act(async () => {});
    expect(listCalls[listCalls.length - 1]).toEqual({ limit: 200, q: undefined });
    expect(host.querySelectorAll('li').length).toBe(151);
    // 150 of 200 asked for: the end.
    expect(host.querySelector('.sidebar-more-button')).toBeNull();
    unmount();
  });
});
