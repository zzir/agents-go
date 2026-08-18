// @vitest-environment jsdom
import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import { act } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { usePage } from '@/lib/hooks';

const g = globalThis as Record<string, unknown>;
let savedActEnv: unknown;
beforeAll(() => { savedActEnv = g.IS_REACT_ACT_ENVIRONMENT; g.IS_REACT_ACT_ENVIRONMENT = true; });
afterAll(() => { if (savedActEnv === undefined) delete g.IS_REACT_ACT_ENVIRONMENT; else g.IS_REACT_ACT_ENVIRONMENT = savedActEnv; });

type Page = ReturnType<typeof usePage<number>>;

// Renders the hook over `all` and exposes what it returned last.
function harness(): { root: Root; last: () => Page; render: (all: number[]) => Promise<void> } {
  let last!: Page;
  function Probe({ all }: { all: number[] }) { last = usePage(all, 3); return null; }
  const root = createRoot(document.createElement('div'));
  return {
    root,
    last: () => last,
    render: async all => { await act(async () => { root.render(<Probe all={all} />); }); },
  };
}

describe('usePage', () => {
  it('slices by page and clamps to the last page when the list shrinks', async () => {
    const h = harness();
    await h.render([1, 2, 3, 4, 5, 6, 7]);
    expect(h.last().count).toBe(3);
    expect(h.last().items).toEqual([1, 2, 3]);

    await act(async () => { h.last().setIndex(2); });
    expect(h.last().index).toBe(2);
    expect(h.last().items).toEqual([7]);

    // The only row of the last page goes: the page before it, not an empty one.
    await h.render([1, 2, 3, 4, 5, 6]);
    expect(h.last().count).toBe(2);
    expect(h.last().index).toBe(1);
    expect(h.last().items).toEqual([4, 5, 6]);

    // …and the clamp sticks when the list grows back.
    await h.render([1, 2, 3, 4, 5, 6, 7]);
    expect(h.last().index).toBe(1);
    expect(h.last().items).toEqual([4, 5, 6]);

    // An empty list is one empty page.
    await h.render([]);
    expect(h.last().count).toBe(1);
    expect(h.last().index).toBe(0);
    expect(h.last().items).toEqual([]);
    await act(async () => { h.root.unmount(); });
  });
});
