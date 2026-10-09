import { useLayoutEffect, useRef } from 'react';
import morphdom from 'morphdom';
import { renderMarkdownLite } from '@/lib/markdown';

// The live streaming block: deltas are morphdom-patched into the existing DOM
// so a selection survives the stream — invariant 18. React never renders
// children into this div; the blinking cursor is CSS (chat.css .streaming).
export function StreamingMarkdown({ text }: { text: string }) {
  const ref = useRef<HTMLDivElement>(null);
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    morphdom(el, `<div>${renderMarkdownLite(text)}</div>`, {
      childrenOnly: true,
      onBeforeElUpdated: (from, to) => {
        if (from.isEqualNode(to)) return false;
        // Growing text tail: assigning nodeValue is a replaceData that collapses
        // a selection range inside the node; appending only the suffix keeps it.
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
