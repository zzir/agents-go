// @vitest-environment jsdom
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { act } from 'react';
import { createRoot } from 'react-dom/client';
// hooks.ts imports useConfirm; Primer's dist drags CSS vitest can't load.
vi.mock('@primer/react', () => ({ useConfirm: () => async () => true }));
import { useResizablePane } from '@/lib/hooks';

const g = globalThis as Record<string, unknown>;
let savedActEnv: unknown;
beforeAll(() => {
  savedActEnv = g.IS_REACT_ACT_ENVIRONMENT;
  g.IS_REACT_ACT_ENVIRONMENT = true;
  // jsdom has no pointer capture: the handle owns the pointer for a whole test.
  Object.assign(Element.prototype, { setPointerCapture() {}, hasPointerCapture() { return true; } });
});
afterAll(() => { if (savedActEnv === undefined) delete g.IS_REACT_ACT_ENVIRONMENT; else g.IS_REACT_ACT_ENVIRONMENT = savedActEnv; });
beforeEach(() => { localStorage.clear(); vi.useFakeTimers(); });
afterEach(() => { vi.useRealTimers(); });

type Options = Parameters<typeof useResizablePane>[0];
type Pane = ReturnType<typeof useResizablePane>;

const SIDEBAR: Options = { storageKey: 'pane', min: 260, max: 400, defaultWidth: 300, edge: 'left', collapsedWidth: 48 };

// Mounts one pane and drives its handle with pointer and key events.
function mount(opts: Options = SIDEBAR) {
  let last!: Pane;
  function Probe() {
    last = useResizablePane(opts);
    return <div tabIndex={0} {...last.handleProps} />;
  }
  const el = document.createElement('div');
  document.body.appendChild(el);
  const root = createRoot(el);
  act(() => root.render(<Probe />));
  const handle = el.querySelector('div')!;
  const pointer = (type: string, clientX: number) => act(() => {
    handle.dispatchEvent(new PointerEvent(type, { bubbles: true, clientX, pointerId: 1, button: 0 }));
  });
  const key = (k: string) => act(() => { handle.dispatchEvent(new KeyboardEvent('keydown', { bubbles: true, key: k })); });
  return { pane: () => last, pointer, key, unmount: () => { act(() => root.unmount()); el.remove(); } };
}

describe('useResizablePane with a collapsed width', () => {
  it('snaps to the rail well inside the minimum and back only past a wider point', () => {
    const { pane, pointer, unmount } = mount();
    pointer('pointerdown', 300);
    pointer('pointermove', 230);
    expect(pane().width).toBe(260);
    expect(pane().collapsed).toBe(false);

    pointer('pointermove', 190);
    expect(pane().collapsed).toBe(true);
    expect(pane().snapping).toBe(true);
    expect(pane().width).toBe(260);
    expect(localStorage.getItem('paneCollapsed')).toBe('1');

    pointer('pointermove', 210);
    expect(pane().collapsed).toBe(true);

    pointer('pointermove', 230);
    expect(pane().collapsed).toBe(false);
    expect(pane().width).toBe(260);
    expect(localStorage.getItem('paneCollapsed')).toBe('0');

    pointer('pointermove', 330);
    expect(pane().width).toBe(330);
    expect(pane().snapping).toBe(false);
    pointer('pointerup', 330);
    expect(localStorage.getItem('pane')).toBe('330');
    unmount();
  });

  it('turns snapping off by itself after the snap', () => {
    const { pane, pointer, unmount } = mount();
    pointer('pointerdown', 300);
    pointer('pointermove', 100);
    expect(pane().snapping).toBe(true);
    act(() => { vi.advanceTimersByTime(250); });
    expect(pane().snapping).toBe(false);
    expect(pane().collapsed).toBe(true);
    unmount();
  });

  it('crosses the minimum with the arrow keys, and expand() restores the kept width', () => {
    const { pane, key, unmount } = mount();
    for (let i = 0; i < 4; i++) key('ArrowLeft');
    expect(pane().width).toBe(260);
    expect(pane().collapsed).toBe(false);
    key('ArrowLeft');
    expect(pane().collapsed).toBe(true);
    key('ArrowLeft');
    expect(pane().collapsed).toBe(true);
    key('ArrowRight');
    expect(pane().collapsed).toBe(false);
    expect(pane().width).toBe(260);

    key('ArrowRight');
    expect(pane().width).toBe(270);
    act(() => pane().expand());
    expect(pane().width).toBe(270);
    unmount();
  });

  it('comes back collapsed, with the width it will expand to', () => {
    localStorage.setItem('pane', '330');
    localStorage.setItem('paneCollapsed', '1');
    const { pane, unmount } = mount();
    expect(pane().collapsed).toBe(true);
    expect(pane().width).toBe(330);
    act(() => pane().expand());
    expect(pane().collapsed).toBe(false);
    expect(pane().snapping).toBe(true);
    expect(pane().width).toBe(330);
    unmount();
  });
});

describe('useResizablePane without one', () => {
  it('stops at the minimum', () => {
    const { pane, pointer, key, unmount } = mount({ ...SIDEBAR, collapsedWidth: undefined });
    pointer('pointerdown', 300);
    pointer('pointermove', 100);
    expect(pane().width).toBe(260);
    expect(pane().collapsed).toBe(false);
    pointer('pointerup', 100);
    key('ArrowLeft');
    expect(pane().width).toBe(260);
    expect(pane().collapsed).toBe(false);
    expect(localStorage.getItem('paneCollapsed')).toBeNull();
    unmount();
  });
});
