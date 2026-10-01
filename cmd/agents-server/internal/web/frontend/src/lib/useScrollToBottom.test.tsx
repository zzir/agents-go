// @vitest-environment jsdom
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest';
import { act, useCallback } from 'react';
import { createRoot, type Root } from 'react-dom/client';
// hooks.ts imports useConfirm; Primer's dist drags CSS vitest can't load.
vi.mock('@primer/react', () => ({ useConfirm: () => async () => true }));
import { useScrollToBottom } from '@/lib/hooks';

const g = globalThis as Record<string, unknown>;
let savedActEnv: unknown;
beforeAll(() => { savedActEnv = g.IS_REACT_ACT_ENVIRONMENT; g.IS_REACT_ACT_ENVIRONMENT = true; });
afterAll(() => { if (savedActEnv === undefined) delete g.IS_REACT_ACT_ENVIRONMENT; else g.IS_REACT_ACT_ENVIRONMENT = savedActEnv; });

// jsdom lays nothing out: the scroll geometry is ours, the observer is a stub
// whose callback the test fires, and the clock is a number the test moves.
let fireResize: (() => void) | null = null;
let clock = 0;
let root: Root | null = null;
let host: HTMLDivElement | null = null;
let frames: FrameRequestCallback[] = [];

// A scroll event is handled a frame later; the test decides when that frame is.
function scrollEvent(node: HTMLElement) {
  act(() => { node.dispatchEvent(new Event('scroll')); });
  const due = frames;
  frames = [];
  act(() => { for (const cb of due) cb(clock); });
}

function mount({ observer = true } = {}) {
  fireResize = null;
  frames = [];
  clock = 10_000;
  vi.spyOn(performance, 'now').mockImplementation(() => clock);
  vi.stubGlobal('requestAnimationFrame', (cb: FrameRequestCallback) => frames.push(cb));
  vi.stubGlobal('cancelAnimationFrame', () => {});
  vi.stubGlobal('ResizeObserver', observer ? class {
    constructor(cb: () => void) { fireResize = cb; }
    observe() {}
    disconnect() { fireResize = null; }
  } : undefined);
  // Like a browser, the position clamps to what the content allows.
  const box = {
    scrollHeight: 1000,
    clientHeight: 400,
    raw: 0,
    get max() { return Math.max(0, this.scrollHeight - this.clientHeight); },
    get scrollTop() { return Math.min(this.raw, this.max); },
    set scrollTop(v: number) { this.raw = Math.max(0, Math.min(v, this.max)); },
    get atBottom() { return this.scrollTop === this.max; },
  };
  const m = {
    anchor: null as unknown as ReturnType<typeof useScrollToBottom>,
    node: null as unknown as HTMLDivElement,
    box,
    // arrive re-renders with a new dep: a message or a streamed delta landed.
    arrive: () => { /* set below */ },
  };
  function Probe({ dep }: { dep: number }) {
    const anchor = useScrollToBottom(dep, 'session-1');
    m.anchor = anchor;
    const hookRef = anchor.ref;
    // Stable on purpose: an inline ref would re-run the hook's ref callback on
    // every render and re-pin by itself, passing these tests for free.
    const ref = useCallback((el: HTMLDivElement | null) => {
      if (el) {
        m.node = el;
        Object.defineProperty(el, 'scrollHeight', { configurable: true, get: () => box.scrollHeight });
        Object.defineProperty(el, 'clientHeight', { configurable: true, get: () => box.clientHeight });
        Object.defineProperty(el, 'scrollTop', { configurable: true, get: () => box.scrollTop, set: (v: number) => { box.scrollTop = v; } });
        el.scrollTo = vi.fn() as unknown as typeof el.scrollTo;
      }
      hookRef(el);
    }, [hookRef]);
    return <div ref={ref}><div className="chat-log" /></div>;
  }
  // In the document: a selection only holds a range that is.
  host = document.body.appendChild(document.createElement('div'));
  root = createRoot(host);
  let dep = 0;
  act(() => { root!.render(<Probe dep={dep} />); });
  m.arrive = () => { dep++; act(() => { root!.render(<Probe dep={dep} />); }); };
  return m;
}

afterEach(() => {
  act(() => { root?.unmount(); });
  root = null;
  host?.remove();
  host = null;
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe('useScrollToBottom', () => {
  it('follows height that arrives with no new message while pinned', () => {
    const m = mount();
    expect(m.box.atBottom).toBe(true);
    m.box.scrollHeight = 1600; // a diagram rendered late
    expect(m.box.atBottom).toBe(false);
    act(() => { fireResize!(); });
    expect(m.box.atBottom).toBe(true);
  });

  it('leaves a reader who wheeled up where they are, until they ask to follow again', () => {
    const m = mount();
    act(() => { m.node.dispatchEvent(new WheelEvent('wheel', { deltaY: -40 })); });
    expect(m.anchor.isSticky).toBe(false);
    m.box.scrollTop = 300;
    clock += 5_000;
    m.box.scrollHeight = 2000;
    act(() => { fireResize!(); });
    expect(m.box.scrollTop).toBe(300);

    act(() => { m.anchor.scrollToBottom(); });
    expect(m.anchor.isSticky).toBe(true);
    expect(m.node.scrollTo).toHaveBeenCalledWith({ top: 2000, behavior: 'smooth' });
    m.box.scrollHeight = 2400;
    act(() => { fireResize!(); });
    expect(m.box.atBottom).toBe(true);
  });

  it('does not follow growth after a press in the log, however late it lands', () => {
    const m = mount();
    // A small block opened at the bottom: the view stays put and stays pinned.
    act(() => { m.node.dispatchEvent(new Event('pointerdown', { bubbles: true })); });
    m.box.scrollHeight = 1050;
    act(() => { fireResize!(); });
    expect(m.box.scrollTop).toBe(600);
    expect(m.anchor.isSticky).toBe(true);
    // Its content lands much later (a slow frame, a lazy load) and pushes the
    // bottom out of reach: still not followed, and following stops.
    clock += 5_000;
    m.box.scrollHeight = 2100;
    act(() => { fireResize!(); });
    expect(m.box.scrollTop).toBe(600);
    expect(m.anchor.isSticky).toBe(false);
    m.box.scrollHeight = 2600;
    act(() => { fireResize!(); });
    expect(m.box.scrollTop).toBe(600);
  });

  it('follows again once new content arrives or the person re-pins', () => {
    const m = mount();
    act(() => { m.node.dispatchEvent(new Event('keydown', { bubbles: true })); });
    m.box.scrollHeight = 1040;
    act(() => { fireResize!(); });
    expect(m.box.scrollTop).toBe(600);

    // A message lands while still pinned: the pin takes it, and what renders
    // late inside it is followed.
    m.box.scrollHeight = 1200;
    m.arrive();
    expect(m.box.atBottom).toBe(true);
    m.box.scrollHeight = 1500;
    act(() => { fireResize!(); });
    expect(m.box.atBottom).toBe(true);

    // The person's own re-pin (their send, Jump to latest) does the same.
    act(() => { m.node.dispatchEvent(new Event('pointerdown', { bubbles: true })); });
    act(() => { m.anchor.scrollToBottom(); });
    m.box.scrollHeight = 1800;
    act(() => { fireResize!(); });
    expect(m.box.atBottom).toBe(true);
  });

  it('does not follow while the person is selecting in the log', () => {
    const m = mount();
    // A selection changing inside the log vetoes the follow.
    m.node.firstElementChild!.textContent = 'some text';
    const range = document.createRange();
    range.selectNodeContents(m.node.firstElementChild!);
    document.getSelection()!.removeAllRanges();
    document.getSelection()!.addRange(range);
    act(() => { document.dispatchEvent(new Event('selectionchange')); });
    // Growth during it is not followed; having pushed the bottom out of
    // reach, it hands over to "Jump to latest" — there is content below now.
    m.box.scrollHeight = 1300;
    act(() => { fireResize!(); });
    expect(m.box.scrollTop).toBe(600);
    expect(m.anchor.isSticky).toBe(false);
    document.getSelection()!.removeAllRanges();
  });

  it('does not take growth under its own pin for the person leaving the bottom', () => {
    const m = mount();
    // The pin's own scroll event is handled a frame later, after more height
    // has landed: the observer follows it, the scroll handler must not unstick.
    m.box.scrollHeight = 1500;
    scrollEvent(m.node);
    expect(m.anchor.isSticky).toBe(true);
    act(() => { fireResize!(); });
    expect(m.box.atBottom).toBe(true);

    // The same holds for the trailing events of a smooth scroll to the bottom.
    act(() => { m.node.dispatchEvent(new WheelEvent('wheel', { deltaY: -40 })); });
    m.box.scrollTop = 200;
    scrollEvent(m.node);
    expect(m.anchor.isSticky).toBe(false);
    clock += 5_000;
    act(() => { m.anchor.scrollToBottom(); });
    m.box.scrollTop = 600; // on its way down, still far from the bottom
    scrollEvent(m.node);
    expect(m.anchor.isSticky).toBe(true);
  });

  it('survives the log emptying and filling again under the pin', () => {
    const m = mount();
    scrollEvent(m.node); // the first pin's own scroll event: seen at the bottom
    // A reload swaps the list through an empty state: the browser clamps the
    // position to 0, and the observer sees the shrink a frame before the scroll
    // event of that clamp is handled — by which time the content is back.
    m.box.scrollHeight = 400;
    expect(m.box.scrollTop).toBe(0);
    act(() => { fireResize!(); });
    m.box.scrollHeight = 2300;
    scrollEvent(m.node);
    expect(m.anchor.isSticky).toBe(true);
    act(() => { fireResize!(); });
    expect(m.box.atBottom).toBe(true);
  });

  it('still reads an upward move as leaving, and without an observer a gap too', () => {
    const m = mount();
    m.box.scrollTop = 300; // a scrollbar drag upwards
    scrollEvent(m.node);
    expect(m.anchor.isSticky).toBe(false);
    act(() => { root!.unmount(); });
    root = null;
    host?.remove();

    // No ResizeObserver in this browser: the scroll handler is the only place
    // that notices the log grew away from the pin.
    const bare = mount({ observer: false });
    expect(fireResize).toBeNull();
    bare.box.scrollHeight = 1500;
    scrollEvent(bare.node);
    expect(bare.anchor.isSticky).toBe(false);
  });

  it('stops observing when the log unmounts', () => {
    mount();
    expect(fireResize).not.toBeNull();
    act(() => { root!.unmount(); });
    root = null;
    expect(fireResize).toBeNull();
  });
});
