import { useEffect, type ReactNode } from 'react';
import { createPortal } from 'react-dom';

// ZoomOverlay shows one thing — a diagram, an image — over the page at its
// natural size, scrollable when it is larger than the window; a click outside
// it or Escape closes it.
export function ZoomOverlay({ children, onClose }: { children: ReactNode; onClose: () => void }) {
  useEffect(() => {
    const h = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose(); };
    document.addEventListener('keydown', h);
    return () => document.removeEventListener('keydown', h);
  }, [onClose]);

  return createPortal(
    <div className="svg-overlay" onClick={onClose}>
      <div onClick={(e) => e.stopPropagation()}>{children}</div>
    </div>,
    document.body,
  );
}
