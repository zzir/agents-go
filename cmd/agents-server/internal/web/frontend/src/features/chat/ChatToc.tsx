import { useEffect, useRef, useState, memo, type CSSProperties, type RefObject } from 'react';
import { Tooltip } from '@primer/react';

export interface TocItem {
  idx: number;
  preview: string;
}

interface ChatTocProps {
  items: TocItem[];
  scrollElRef: RefObject<HTMLElement | null>;
  onJump: (idx: number) => void;
}

// Bar widths by distance from the pointed bar: the pointed one longest, its
// neighbours shorter step by step, the rest at rest (chat.css's default).
const BAR_WIDTHS = ['36px', '28px', '22px', '18px'];

function barWidth(k: number, pointed: number | null): string | undefined {
  if (pointed === null) return undefined;
  return BAR_WIDTHS[Math.abs(k - pointed)];
}

// Left-rail minimap of the user's prompts: one bar per message, the active bar
// tracks the scroll position, hover shows the prompt's first line, click jumps
// to it; pointing at a bar fisheyes it and its neighbours. chat.css hides it on
// narrow columns.
export const ChatToc = memo(function ChatToc({ items, scrollElRef, onJump }: ChatTocProps) {
  const [active, setActive] = useState(0);
  const [pointed, setPointed] = useState<number | null>(null);
  const itemsRef = useRef(items);
  itemsRef.current = items;

  useEffect(() => {
    const el = scrollElRef.current;
    if (!el) return;
    let rafId = 0;
    const recompute = () => {
      rafId = 0;
      const list = itemsRef.current;
      const top = el.getBoundingClientRect().top;
      // Active = last prompt above the upper third of the viewport.
      const threshold = top + el.clientHeight / 3;
      let cur = 0;
      for (let k = 0; k < list.length; k++) {
        const node = el.querySelector(`[data-msg-idx="${list[k].idx}"]`);
        if (node && node.getBoundingClientRect().top <= threshold) cur = k;
      }
      setActive(cur);
    };
    const onScroll = () => { if (!rafId) rafId = requestAnimationFrame(recompute); };
    el.addEventListener('scroll', onScroll, { passive: true });
    recompute();
    return () => {
      if (rafId) cancelAnimationFrame(rafId);
      el.removeEventListener('scroll', onScroll);
    };
  }, [scrollElRef, items.length]);

  if (items.length < 2) return null;

  return (
    // Magnification ends when the pointer or focus leaves the RAIL, not the bar:
    // a click leaves focus on its bar, and focus moving bar to bar keeps it.
    <nav
      className="chat-toc"
      aria-label="Session outline"
      onMouseLeave={() => setPointed(null)}
      onBlur={e => { if (!e.currentTarget.contains(e.relatedTarget as Node | null)) setPointed(null); }}
    >
      {items.map((it, k) => (
        <Tooltip key={it.idx} text={it.preview} direction="e" type="label">
          <button
            className={'chat-toc-item' + (k === active ? ' active' : '')}
            style={{ '--toc-w': barWidth(k, pointed) } as CSSProperties}
            onMouseEnter={() => setPointed(k)}
            onFocus={() => setPointed(k)}
            onClick={() => onJump(it.idx)}
          >
            <span className="chat-toc-bar" />
          </button>
        </Tooltip>
      ))}
    </nav>
  );
});
