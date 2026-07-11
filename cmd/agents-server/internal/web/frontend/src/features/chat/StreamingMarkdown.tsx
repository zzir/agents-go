import { useLayoutEffect, useRef } from 'react';
import morphdom from 'morphdom';
import { renderMarkdownLite } from '@/lib/markdown';

// The live streaming block. Each delta is patched into the existing DOM with
// morphdom instead of rewriting innerHTML wholesale: settled blocks keep their
// node identity and the growing tail is a nodeValue update, so a text
// selection anywhere in the block survives the stream. React never renders
// children into this div — the DOM below it belongs to morphdom. The blinking
// cursor is CSS (chat.css .streaming), not part of the markdown source.
export function StreamingMarkdown({ text }: { text: string }) {
  const ref = useRef<HTMLDivElement>(null);
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    morphdom(el, `<div>${renderMarkdownLite(text)}</div>`, {
      childrenOnly: true,
      onBeforeElUpdated: (from, to) => {
        if (from.isEqualNode(to)) return false;
        // Growing text tail: morphdom assigns nodeValue wholesale, which the
        // DOM treats as replaceData(0, len, …) and collapses any selection
        // range inside the node. Appending just the suffix leaves existing
        // range boundaries untouched, so a selection in the paragraph the
        // model is still writing survives.
        const a = from.firstChild;
        const b = to.firstChild;
        if (
          from.childNodes.length === 1 && a instanceof Text &&
          to.childNodes.length === 1 && b instanceof Text &&
          b.data.startsWith(a.data)
        ) {
          if (b.data.length > a.data.length) a.appendData(b.data.slice(a.data.length));
          return false;
        }
        return true;
      },
    });
  }, [text]);
  return <div ref={ref} className="turn-text markdown-body streaming" />;
}
