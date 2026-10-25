// @vitest-environment jsdom
import { describe, it, expect, beforeAll, afterAll, afterEach, vi } from 'vitest';
import { act, useRef, type ElementType } from 'react';
import { createRoot } from 'react-dom/client';

vi.mock('@primer/react', () => ({
  IconButton: ({ icon: Icon, onClick, className, 'aria-label': label }: { icon: ElementType; onClick?: () => void; className?: string; 'aria-label'?: string }) => (
    <button type="button" className={className} aria-label={label} onClick={onClick}><Icon /></button>
  ),
}));
vi.mock('@primer/octicons-react', () => ({ ArrowDownIcon: () => <i className="arrow" /> }));
vi.mock('@/features/chat/ChecklistChip', () => ({ ChecklistChip: () => <span className="chip" /> }));
import { ComposerDock } from '@/features/chat/ComposerDock';

const g = globalThis as Record<string, unknown>;
let savedActEnv: unknown;
beforeAll(() => { savedActEnv = g.IS_REACT_ACT_ENVIRONMENT; g.IS_REACT_ACT_ENVIRONMENT = true; });
afterAll(() => { if (savedActEnv === undefined) delete g.IS_REACT_ACT_ENVIRONMENT; else g.IS_REACT_ACT_ENVIRONMENT = savedActEnv; });

function Harness({ following, responding, onJump }: { following: boolean; responding: boolean; onJump: () => void }) {
  const transcriptRef = useRef<HTMLDivElement>(null);
  return (
    <div className="area">
      <div ref={transcriptRef} className="transcript"><p>history</p></div>
      <ComposerDock
        following={following}
        transcriptRef={transcriptRef}
        responding={responding}
        onJump={onJump}
        rows={<div className="row">bar</div>}
        composer={compact => <div className="box chat-input-box" data-compact={String(compact)}><textarea /><button type="button">stop</button></div>}
      />
    </div>
  );
}

function mount(following: boolean) {
  const host = document.createElement('div');
  document.body.appendChild(host);
  const root = createRoot(host);
  const onJump = vi.fn();
  const render = (f: boolean, responding = false) => act(() => { root.render(<Harness following={f} responding={responding} onJump={onJump} />); });
  render(following);
  const q = (sel: string) => host.querySelector(sel) as HTMLElement;
  const fire = (sel: string, type: string, init: EventInit & { relatedTarget?: EventTarget } = {}) => act(() => {
    const ev = type.startsWith('focus') ? new FocusEvent(type, { bubbles: true, ...init }) : new Event(type, { bubbles: true, ...init });
    q(sel).dispatchEvent(ev);
  });
  // The transcript's geometry, a 600px view: below = how far its bottom lies
  // under the view.
  const geo: Record<string, number> = { scrollHeight: 3000, clientHeight: 600, scrollTop: 2400 };
  for (const k of Object.keys(geo)) Object.defineProperty(q('.transcript'), k, { configurable: true, get: () => geo[k] });
  const scrollTo = (below: number) => {
    geo.scrollTop = geo.scrollHeight - geo.clientHeight - below;
    fire('.transcript', 'scroll');
  };
  return {
    host, onJump, render, q, fire, scrollTo,
    readBack: () => { render(false); scrollTo(400); },
    compact: () => q('.box').dataset.compact === 'true',
    unmount: () => { act(() => root.unmount()); host.remove(); },
  };
}

describe('ComposerDock', () => {
  // Reading back starts a third of the view up, and holds until following:
  // Jump and the fold are its two faces.
  it('folds once a third of the view lies below, until following again', () => {
    const m = mount(true);
    m.render(false);
    m.scrollTo(150);
    expect(m.compact()).toBe(false);
    expect(m.q('.dock-jump')).toBeNull();
    m.scrollTo(250);
    expect(m.compact()).toBe(true);
    m.scrollTo(100);
    expect(m.compact()).toBe(true);
    act(() => m.q('.dock-jump').click());
    expect(m.onJump).toHaveBeenCalledTimes(1);
    m.render(true);
    expect(m.compact()).toBe(false);
    expect(m.q('.dock-jump')).toBeNull();
    m.render(false);
    expect(m.compact()).toBe(false);
    m.unmount();
  });

  // Jump is an arrow, or the dots while a run goes on; the checklist belongs
  // to the unfolded composer.
  it('shows each chip with its state', () => {
    const m = mount(true);
    expect(m.q('.chip')).not.toBeNull();
    m.readBack();
    expect(m.q('.chip')).toBeNull();
    expect(m.q('.dock-jump').getAttribute('aria-label')).toBe('Jump to latest');
    expect(m.q('.dock-jump .arrow')).not.toBeNull();
    m.render(false, true);
    expect(m.q('.dock-jump').getAttribute('aria-label')).toBe('Responding — jump to latest');
    expect(m.q('.dock-jump .thinking-dots')).not.toBeNull();
    m.fire('textarea', 'input');
    expect(m.q('.chip')).not.toBeNull();
    expect(m.q('.dock-jump')).not.toBeNull();
    m.unmount();
  });

  it('opens on work in the composer and folds again on a press in the transcript', () => {
    const m = mount(true);
    m.readBack();
    m.fire('textarea', 'input');
    expect(m.compact()).toBe(false);
    m.fire('.transcript p', 'pointerdown');
    expect(m.compact()).toBe(true);
    m.fire('textarea', 'pointerdown');
    expect(m.compact()).toBe(false);
    m.unmount();
  });

  // Stop pressed while reading back stops the run; it does not open the box.
  it('leaves a press on a button folded', () => {
    const m = mount(true);
    m.readBack();
    m.fire('.box button', 'pointerdown');
    expect(m.compact()).toBe(true);
    m.unmount();
  });

  // Window refocus fires focus with no element it came from.
  it('opens on keyboard focus, not on a window refocus', () => {
    const m = mount(true);
    m.readBack();
    m.fire('textarea', 'focusin');
    expect(m.compact()).toBe(true);
    // Pressing Stop moves focus from the draft to the button: still folded.
    m.fire('.box button', 'focusin', { relatedTarget: m.q('textarea') });
    expect(m.compact()).toBe(true);
    m.fire('textarea', 'focusin', { relatedTarget: m.q('.row') });
    expect(m.compact()).toBe(false);
    m.unmount();
  });

  // Focus stays in the textarea after a send, so work done before reading
  // back must not keep the box open once the person scrolls back.
  it('starts every read-back folded', () => {
    const m = mount(true);
    m.fire('textarea', 'input');
    m.render(false);
    m.scrollTo(100);
    m.fire('textarea', 'input');
    m.scrollTo(400);
    expect(m.compact()).toBe(true);
    m.fire('textarea', 'input');
    m.render(true);
    m.readBack();
    expect(m.compact()).toBe(true);
    m.unmount();
  });
});

describe('ComposerDock height', () => {
  let observers: (() => void)[] = [];
  let height = 0;
  let chip = 0;
  let boxHeight = 0;
  const savedRO = g.ResizeObserver;
  const savedOffset = Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'offsetHeight');
  const observe = () => act(() => observers.forEach(cb => cb()));
  beforeAll(() => {
    g.ResizeObserver = class { constructor(cb: () => void) { observers.push(cb); } observe() {} disconnect() {} };
    Object.defineProperty(HTMLElement.prototype, 'offsetHeight', {
      configurable: true,
      get(this: HTMLElement) {
        if (this.classList.contains('composer-dock')) return height;
        if (this.classList.contains('chat-input-box')) return boxHeight;
        return this.classList.contains('composer-dock-start') ? chip : 0;
      },
    });
  });
  afterAll(() => {
    g.ResizeObserver = savedRO;
    if (savedOffset) Object.defineProperty(HTMLElement.prototype, 'offsetHeight', savedOffset);
  });
  afterEach(() => { observers = []; });

  // The room the transcript keeps is the resting height: a fold never
  // changes it, so folding never moves the scroll.
  it('keeps --dock-h at the unfolded height', () => {
    height = 120;
    const m = mount(true);
    const dockH = () => m.q('.area').style.getPropertyValue('--dock-h');
    expect(dockH()).toBe('120px');
    m.readBack();
    height = 48;
    observe();
    expect(dockH()).toBe('120px');
    m.fire('textarea', 'input');
    height = 140;
    observe();
    expect(dockH()).toBe('140px');
    const area = m.q('.area');
    m.unmount();
    expect(area.style.getPropertyValue('--dock-h')).toBe('');
  });

  // The fold eases the box from one layout's height to the other's, and the
  // room stays put while it runs.
  it('eases the fold and holds --dock-h through it', () => {
    const anims: { keyframes: unknown; onfinish: (() => void) | null; oncancel: (() => void) | null; cancelled: boolean }[] = [];
    const proto = HTMLElement.prototype as unknown as Record<string, unknown>;
    proto.animate = function (keyframes: unknown) {
      const a = { keyframes, onfinish: null, oncancel: null, cancelled: false, cancel() { a.cancelled = true; boxHeight = 104; } };
      anims.push(a);
      return a;
    };
    height = 120;
    boxHeight = 104;
    const m = mount(true);
    boxHeight = 44;
    m.readBack();
    expect(anims.map(a => a.keyframes)).toEqual([{ height: ['104px', '44px'] }]);
    expect(m.q('.box').classList.contains('is-folding')).toBe(true);
    height = 90;
    observe();
    expect(m.q('.area').style.getPropertyValue('--dock-h')).toBe('120px');
    // Reversed part way (the box is 60px tall): the first fold is dropped
    // without a word, the second starts from 60px.
    boxHeight = 60;
    m.fire('textarea', 'input');
    expect(anims[0].cancelled).toBe(true);
    expect(anims[0].onfinish).toBeNull();
    expect(anims.map(a => a.keyframes)).toEqual([{ height: ['104px', '44px'] }, { height: ['60px', '104px'] }]);
    expect(m.q('.box').classList.contains('is-folding')).toBe(true);
    expect(m.q('.area').style.getPropertyValue('--dock-h')).toBe('120px');
    act(() => anims[1].onfinish?.());
    expect(m.q('.box').classList.contains('is-folding')).toBe(false);
    expect(m.q('.area').style.getPropertyValue('--dock-h')).toBe('90px');
    m.unmount();
    delete proto.animate;
    boxHeight = 0;
  });

  // A checklist chip stays above the dock while following: the room reaches up to it.
  it('reaches up to the checklist chip', () => {
    height = 120;
    chip = 28;
    const rect = (top: number, bottom: number) => ({ top, bottom }) as DOMRect;
    const saved = HTMLElement.prototype.getBoundingClientRect;
    HTMLElement.prototype.getBoundingClientRect = function (this: HTMLElement) {
      return this.classList.contains('composer-dock-start') ? rect(644, 672) : rect(680, 800);
    };
    const m = mount(true);
    expect(m.q('.area').style.getPropertyValue('--dock-h')).toBe('156px');
    m.unmount();
    HTMLElement.prototype.getBoundingClientRect = saved;
    chip = 0;
  });
});
