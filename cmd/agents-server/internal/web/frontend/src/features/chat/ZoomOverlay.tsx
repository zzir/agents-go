import { useEffect, useRef, type ReactNode } from 'react';
import { createPortal } from 'react-dom';

// ZoomOverlay shows one thing — a diagram, an image — over the page at its
// natural size, scrollable when it is larger than the window; a click outside
// it or Escape closes it. It is a modal dialog to assistive tech: focus moves
// in on open, Tab stays inside, and the opener gets focus back on close.
export function ZoomOverlay({ children, onClose }: { children: ReactNode; onClose: () => void }) {
  const boxRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const opener = document.activeElement as HTMLElement | null;
    boxRef.current?.focus();
    const h = (e: KeyboardEvent) => {
      if (e.key === 'Escape') { onClose(); return; }
      if (e.key !== 'Tab') return;
      // The content is usually a lone SVG with nothing focusable — cycle
      // within whatever there is, or keep focus on the box itself.
      const box = boxRef.current;
      if (!box) return;
      const focusables = box.querySelectorAll<HTMLElement>('a[href], button, input, textarea, select, [tabindex]:not([tabindex="-1"])');
      if (focusables.length === 0) { e.preventDefault(); return; }
      const first = focusables[0], last = focusables[focusables.length - 1];
      if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
      else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
    };
    document.addEventListener('keydown', h);
    return () => {
      document.removeEventListener('keydown', h);
      opener?.focus?.();
    };
  }, [onClose]);

  return createPortal(
    <div className="svg-overlay" onClick={onClose}>
      <div ref={boxRef} role="dialog" aria-modal="true" aria-label="Zoomed view" tabIndex={-1}
        onClick={(e) => e.stopPropagation()}>{children}</div>
    </div>,
    document.body,
  );
}
