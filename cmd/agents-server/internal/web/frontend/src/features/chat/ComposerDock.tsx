import { useEffect, useLayoutEffect, useRef, useState, type ReactNode, type RefObject } from 'react';
import { IconButton } from '@primer/react';
import { ArrowDownIcon } from '@primer/octicons-react';
import { ChecklistChip } from '@/features/chat/ChecklistChip';
import { ThinkingDots } from '@/features/chat/ThinkingDots';

interface ComposerDockProps {
  // Whether the transcript follows its bottom (useScrollToBottom's isSticky).
  following: boolean;
  // The transcript's scroll element: its distance from the bottom decides
  // reading back, and a press there hands attention back to it.
  transcriptRef: RefObject<HTMLElement | null>;
  // A run is going: Jump shows the dots instead of the arrow.
  responding: boolean;
  onJump: () => void;
  // Rows that sit with the composer, above it: workflow bars, queued messages.
  rows: ReactNode;
  composer: (compact: boolean) => ReactNode;
}

// ComposerDock floats the bottom of a loaded transcript over it: chips above,
// then the rows and the composer, which folds to one line while the person
// reads back. Each chip shows with its state: the checklist with the unfolded
// composer, Jump while reading back — invariant 95.
export function ComposerDock({ following, transcriptRef, responding, onJump, rows, composer }: ComposerDockProps) {
  const dockRef = useRef<HTMLDivElement>(null);
  const startRef = useRef<HTMLDivElement>(null);
  const composerRef = useRef<HTMLDivElement>(null);
  const followingRef = useRef(following);
  followingRef.current = following;
  // Reading back: not following, and once more than a third of the visible
  // transcript lay below the view; it holds until following again.
  const [away, setAway] = useState(false);
  const readingBack = !following && away;
  // Set by working in the folded composer; cleared by a press in the
  // transcript or by reading back ending.
  const [opened, setOpened] = useState(false);
  const compact = readingBack && !opened;
  const compactRef = useRef(compact);
  compactRef.current = compact;
  const dockH = useRef(0);
  const checkAway = useRef(() => {});
  const measure = useRef(() => {});
  const folding = useRef(false);
  const boxH = useRef(0);

  useEffect(() => {
    const el = transcriptRef.current;
    if (!el) return;
    const check = () => {
      if (followingRef.current) return;
      const dist = el.scrollHeight - el.scrollTop - el.clientHeight;
      if (dist > (el.clientHeight - dockH.current) / 3) setAway(true);
    };
    checkAway.current = check;
    const release = () => setOpened(false);
    el.addEventListener('scroll', check, { passive: true });
    el.addEventListener('pointerdown', release, { passive: true });
    // Content arriving below moves the bottom away with no scroll event.
    const ro = typeof ResizeObserver === 'undefined' ? null : new ResizeObserver(check);
    ro?.observe(el);
    if (el.firstElementChild) ro?.observe(el.firstElementChild);
    return () => {
      el.removeEventListener('scroll', check);
      el.removeEventListener('pointerdown', release);
      ro?.disconnect();
    };
  }, [transcriptRef]);

  useEffect(() => { if (following) setAway(false); else checkAway.current(); }, [following]);
  useEffect(() => { if (!readingBack) setOpened(false); }, [readingBack]);

  // The transcript keeps room for the dock at its resting height (--dock-h on
  // the area), up to the checklist chip when there is one; a fold, folded or
  // under way, leaves it alone, so folding never moves the scroll.
  useLayoutEffect(() => {
    const el = dockRef.current;
    const start = startRef.current;
    const area = el?.parentElement;
    if (!el || !start || !area || typeof ResizeObserver === 'undefined') return;
    const apply = () => area.style.setProperty('--dock-h', `${dockH.current}px`);
    measure.current = () => {
      if (folding.current) return;
      const box = composerRef.current?.querySelector<HTMLElement>('.chat-input-box');
      if (box) boxH.current = box.offsetHeight;
      if (compactRef.current) return;
      dockH.current = start.offsetHeight ? el.getBoundingClientRect().bottom - start.getBoundingClientRect().top : el.offsetHeight;
      apply();
    };
    measure.current();
    const ro = new ResizeObserver(() => measure.current());
    ro.observe(el);
    ro.observe(start);
    return () => { ro.disconnect(); area.style.removeProperty('--dock-h'); };
  }, []);

  // The fold eases the box's height between its two layouts; its rows sit on
  // the bottom edge, so the top edge moves and the send cell stays. A fold
  // reversed under way starts from where it is.
  const anim = useRef<Animation | null>(null);
  useLayoutEffect(() => {
    const box = composerRef.current?.querySelector<HTMLElement>('.chat-input-box');
    if (!box || typeof box.animate !== 'function') return;
    const settle = () => {
      anim.current = null;
      folding.current = false;
      box.classList.remove('is-folding');
      measure.current();
    };
    // Read before the cancel snaps the height; a cancel reports late, so its
    // handlers go first.
    const prev = anim.current;
    const from = prev ? box.offsetHeight : boxH.current;
    if (prev) { prev.onfinish = null; prev.oncancel = null; prev.cancel(); }
    const to = box.offsetHeight;
    boxH.current = to;
    if (!from || from === to || window.matchMedia?.('(prefers-reduced-motion: reduce)').matches) {
      if (prev) settle();
      return;
    }
    const css = getComputedStyle(box);
    folding.current = true;
    box.classList.add('is-folding');
    const a = box.animate({ height: [`${from}px`, `${to}px`] }, {
      duration: parseFloat(css.getPropertyValue('--base-duration-200')) || 200,
      easing: css.getPropertyValue('--base-easing-easeOut').trim() || 'ease-out',
    });
    anim.current = a;
    a.onfinish = settle;
    a.oncancel = settle;
  }, [compact]);
  useEffect(() => () => {
    const a = anim.current;
    if (a) { a.onfinish = null; a.oncancel = null; a.cancel(); }
  }, []);

  const open = () => { if (readingBack) setOpened(true); };

  return (
    <div ref={dockRef} className="composer-dock">
      <div className="composer-dock-float">
        <div ref={startRef} className="composer-dock-start">{!compact && <ChecklistChip />}</div>
        {readingBack && (
          <IconButton
            size="small"
            icon={responding ? ThinkingDots : ArrowDownIcon}
            aria-label={responding ? 'Responding — jump to latest' : 'Jump to latest'}
            className="dock-chip dock-jump"
            onClick={onJump}
          />
        )}
      </div>
      <div className="composer-dock-col">
        {rows}
        {/* Send and stop act where they are; any other press opens the box.
            Focus counts only arriving in the draft from outside the box: a
            window refocus has no element it came from. */}
        <div
          ref={composerRef}
          onPointerDown={e => { if (!(e.target as Element).closest('button')) open(); }}
          onInput={open}
          onPaste={open}
          onFocus={e => {
            if (e.target instanceof HTMLTextAreaElement && e.relatedTarget && !e.currentTarget.contains(e.relatedTarget as Node)) open();
          }}
        >
          {composer(compact)}
        </div>
      </div>
    </div>
  );
}
