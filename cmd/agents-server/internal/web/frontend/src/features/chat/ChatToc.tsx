import { useEffect, useRef, useState, memo, type RefObject } from 'react';
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

// Left-rail minimap of the user's prompts: one bar per message, the active
// bar tracks the scroll position, hovering shows the prompt's first line in
// a Primer tooltip, clicking jumps to the message. Hidden by chat.css when
// the chat column is too narrow for the rail to sit in the side gutter.
export const ChatToc = memo(function ChatToc({ items, scrollElRef, onJump }: ChatTocProps) {
  const [active, setActive] = useState(0);
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
    <nav className="chat-toc" aria-label="Conversation outline">
      {items.map((it, k) => (
        <Tooltip key={it.idx} text={it.preview} direction="e" type="label">
          <button
            className={'chat-toc-item' + (k === active ? ' active' : '')}
            onClick={() => onJump(it.idx)}
          >
            <span className="chat-toc-bar" />
          </button>
        </Tooltip>
      ))}
    </nav>
  );
});
