// @vitest-environment jsdom
import { afterAll, beforeAll, describe, expect, it, vi } from 'vitest';
import { act, type ReactNode } from 'react';
import { createRoot } from 'react-dom/client';

// Primer ships CSS the node loader cannot import: the pieces the scaffold
// lays out are bare elements here.
vi.mock('@primer/react', () => {
  const box = () => ({ children }: { children?: ReactNode }) => <div>{children}</div>;
  const PageHeader = Object.assign(box(), { TitleArea: box(), Title: box(), Actions: box(), Description: box() });
  const ActionList = Object.assign(box(), { Item: box() });
  return {
    ActionList, PageHeader, Stack: box(), Label: box(),
    Button: (p: Record<string, unknown>) => <button type="button" onClick={p.onClick as () => void}>{p.children as ReactNode}</button>,
    Flash: (p: Record<string, unknown>) => <div role="alert">{p.children as ReactNode}</div>,
    TextInput: () => <input />,
    useConfirm: () => async () => true,
  };
});
vi.mock('@primer/react/experimental', () => {
  const box = () => ({ children }: { children?: ReactNode }) => <div>{children}</div>;
  const Blankslate = Object.assign(({ children }: { children?: ReactNode }) => <div data-blankslate>{children}</div>, { Description: box() });
  return { Blankslate, SkeletonText: () => <div /> };
});
import { CrudPanel } from './CrudPanel';

const g = globalThis as Record<string, unknown>;
let savedActEnv: unknown;
beforeAll(() => { savedActEnv = g.IS_REACT_ACT_ENVIRONMENT; g.IS_REACT_ACT_ENVIRONMENT = true; });
afterAll(() => { if (savedActEnv === undefined) delete g.IS_REACT_ACT_ENVIRONMENT; else g.IS_REACT_ACT_ENVIRONMENT = savedActEnv; });

type Props = Partial<Parameters<typeof CrudPanel>[0]>;

async function mount(props: Props, rows: ReactNode = null) {
  const el = document.createElement('div');
  document.body.appendChild(el);
  const root = createRoot(el);
  await act(async () => root.render(
    <CrudPanel title="Agents" onAdd={() => undefined} form={null} isEmpty empty="No agents yet." {...props}>{rows}</CrudPanel>,
  ));
  return {
    alert: () => el.querySelector('[role="alert"]'),
    blankslate: () => el.querySelector('[data-blankslate]'),
    row: () => el.querySelector('[data-row]'),
    retry: () => [...el.querySelectorAll('button')].find(b => b.textContent === 'Retry'),
    unmount: () => act(async () => { root.unmount(); el.remove(); }),
  };
}

describe('CrudPanel load error', () => {
  it('says the read failed, offers Retry, and never shows the empty state', async () => {
    const onRetry = vi.fn();
    const p = await mount({ error: '502 bad gateway', onRetry });
    expect(p.alert()?.textContent).toBe('Could not load agents: 502 bad gatewayRetry');
    expect(p.blankslate()).toBeNull();
    await act(async () => { p.retry()!.click(); });
    expect(onRetry).toHaveBeenCalledTimes(1);
    await p.unmount();
  });

  it('keeps stale rows under the line', async () => {
    const p = await mount({ error: 'offline', isEmpty: false }, <div data-row />);
    expect(p.alert()).not.toBeNull();
    expect(p.row()).not.toBeNull();
    await p.unmount();
  });

  it('names the list by its noun when the title is not the plural', async () => {
    const p = await mount({ title: 'Memory', noun: 'memories', error: 'offline' });
    expect(p.alert()?.textContent).toContain('Could not load memories: offline');
    await p.unmount();
  });

  it('shows the empty state when the read succeeded with nothing', async () => {
    const p = await mount({});
    expect(p.alert()).toBeNull();
    expect(p.blankslate()?.textContent).toBe('No agents yet.');
    await p.unmount();
  });
});
