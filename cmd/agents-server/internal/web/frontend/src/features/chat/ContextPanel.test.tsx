// @vitest-environment jsdom
import { describe, it, expect, beforeAll, afterAll, vi } from 'vitest';
import { act, useEffect, useState, type ReactNode } from 'react';
import { createRoot } from 'react-dom/client';

// Primer ships CSS the node loader cannot import; the panel's pieces are plain
// elements here, and the confirm dialog answers whatever the test decides.
const { answer, confirmSpy, deleteSpy } = vi.hoisted(() => {
  const answer = { value: false };
  return { answer, confirmSpy: vi.fn(async () => answer.value), deleteSpy: vi.fn(async () => null) };
});
vi.mock('@primer/react', () => ({
  Link: ({ children, onClick }: { children?: ReactNode; onClick?: () => void }) => <button type="button" onClick={onClick}>{children}</button>,
  ProgressBar: () => null,
  useConfirm: () => confirmSpy,
}));
vi.mock('@primer/react/experimental', () => {
  const Blankslate = ({ children }: { children?: ReactNode }) => <div>{children}</div>;
  Blankslate.Description = ({ children }: { children?: ReactNode }) => <p>{children}</p>;
  return { Blankslate };
});
vi.mock('@primer/octicons-react', () => ({ MeterIcon: () => null }));
vi.mock('@/layout/SidePanel', () => ({ SidePanel: ({ children }: { children?: ReactNode }) => <div>{children}</div> }));
vi.mock('@/components/Loading', () => ({ Loading: () => <div>loading</div> }));
vi.mock('@/lib/api', () => ({
  api: {
    sessions: {
      context: async () => ({ input_tokens: 100, output_tokens: 10, cached_tokens: 0, cache_write_tokens: 0, session_input_tokens: 100, session_output_tokens: 10, compaction_enabled: false, compaction_tokens: 0 }),
      memory: async () => [{ id: 'm1', key: 'notes', bytes: 12, written_by: 'model', updated_at: 't1' }],
      memoryKey: async () => ({ content: 'remember this' }),
    },
    memories: { delete: deleteSpy },
  },
}));
// The real useApi caches by key and reloads on a subscription bus; what the
// panel needs here is a fetch that lands in state.
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
import { ContextPanel } from '@/features/chat/ContextPanel';

const g = globalThis as Record<string, unknown>;
let savedActEnv: unknown;
beforeAll(() => { savedActEnv = g.IS_REACT_ACT_ENVIRONMENT; g.IS_REACT_ACT_ENVIRONMENT = true; });
afterAll(() => { if (savedActEnv === undefined) delete g.IS_REACT_ACT_ENVIRONMENT; else g.IS_REACT_ACT_ENVIRONMENT = savedActEnv; });

async function mount(onSettingsOpen?: (tab?: string) => void) {
  const host = document.createElement('div');
  document.body.appendChild(host);
  const root = createRoot(host);
  await act(async () => {
    root.render(<ContextPanel sessionId="s1" running={false} onClose={() => {}} onSettingsOpen={onSettingsOpen} />);
  });
  await act(async () => {});
  return { host, unmount: () => { act(() => root.unmount()); host.remove(); } };
}

describe('ContextPanel', () => {
  it('deletes a session memory only through the confirm dialog', async () => {
    const { host, unmount } = await mount();
    const del = host.querySelector('button[aria-label="Delete memory notes"]') as HTMLButtonElement | null;
    expect(del).not.toBeNull();
    answer.value = false;
    await act(async () => { del!.click(); });
    expect(confirmSpy).toHaveBeenCalledTimes(1);
    expect(deleteSpy).not.toHaveBeenCalled();
    answer.value = true;
    await act(async () => { del!.click(); });
    expect(deleteSpy).toHaveBeenCalledWith('m1');
    unmount();
  });

  it('links the missing-window hint to the Agents tab', async () => {
    const open = vi.fn();
    const { host, unmount } = await mount(open);
    const link = [...host.querySelectorAll('button')].find(b => b.textContent === 'Settings → Agents');
    expect(link).toBeDefined();
    await act(async () => { link!.click(); });
    expect(open).toHaveBeenCalledWith('agents');
    unmount();
  });
});
