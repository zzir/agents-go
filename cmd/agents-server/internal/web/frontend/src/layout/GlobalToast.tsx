import React, { useState, useCallback, useEffect, useRef } from 'react';
import { Flash, IconButton } from '@primer/react';
import { XCircleFillIcon, AlertFillIcon, CheckCircleFillIcon, InfoIcon, XIcon } from '@primer/octicons-react';
import { onToast } from '@/lib/toast';

type FlashProps = React.ComponentProps<typeof Flash>;

const FLASH_VARIANT: Record<string, FlashProps['variant']> = { error: 'danger', warning: 'warning', success: 'success', info: 'default' };
const FLASH_ICON: Record<string, React.ReactNode> = {
  error: <XCircleFillIcon size={16} />,
  warning: <AlertFillIcon size={16} />,
  success: <CheckCircleFillIcon size={16} />,
  info: <InfoIcon size={16} />,
};

// A queue, not one slot: three errors during a long run stack up instead of
// each overwriting the last. Errors linger (10s) so they can be read, then
// auto-dismiss; a click, their close button, or Escape (the newest first) takes
// one sooner. The stack div always exists so the live region is established
// before the first announcement.
export function GlobalToast() {
  const [items, setItems] = useState<Array<{ id: number; msg: string; type: string; exiting?: boolean }>>([]);
  const seqRef = useRef(0);
  const timersRef = useRef<Map<number, ReturnType<typeof setTimeout>>>(new Map());
  const itemsRef = useRef(items);
  itemsRef.current = items;

  const dismiss = useCallback((id: number) => {
    const t = timersRef.current.get(id);
    if (t) { clearTimeout(t); timersRef.current.delete(id); }
    setItems(prev => prev.map(it => (it.id === id ? { ...it, exiting: true } : it)));
    setTimeout(() => setItems(prev => prev.filter(it => it.id !== id)), 150);
  }, []);

  useEffect(() => {
    onToast(({ msg, type }) => {
      // Collapse a repeat of a toast that's still on screen: a double-click on
      // Save shouldn't stack two identical errors — the visible one just gets
      // its dismiss timer refreshed.
      const ttl = type === 'error' ? 10000 : 4000;
      const dup = itemsRef.current.find(it => !it.exiting && it.msg === msg && it.type === type);
      if (dup) {
        const prev = timersRef.current.get(dup.id);
        if (prev) clearTimeout(prev);
        timersRef.current.set(dup.id, setTimeout(() => dismiss(dup.id), ttl));
        return;
      }
      const id = ++seqRef.current;
      setItems(prev => [...prev.slice(-4), { id, msg, type }]);
      timersRef.current.set(id, setTimeout(() => dismiss(id), ttl));
    });
    const timers = timersRef.current;
    return () => {
      onToast(null);
      for (const t of timers.values()) clearTimeout(t);
    };
  }, [dismiss]);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape' || e.defaultPrevented) return;
      const last = [...itemsRef.current].reverse().find(it => !it.exiting);
      if (last) dismiss(last.id);
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [dismiss]);

  return (
    <div className="global-toast-stack" role="status" aria-live="polite">
      {items.map(it => (
        <Flash
          key={it.id}
          variant={FLASH_VARIANT[it.type] || 'default'}
          role={it.type === 'error' ? 'alert' : undefined}
          className={'global-toast' + (it.exiting ? ' global-toast-exit' : '')}
          onClick={() => dismiss(it.id)}
        >
          <span className="global-toast-body" title={it.msg}>
            {FLASH_ICON[it.type]}<span className="global-toast-msg">{it.msg}</span>
          </span>
          {it.type === 'error' && (
            <IconButton icon={XIcon} size="small" variant="invisible" aria-label="Dismiss" className="global-toast-close"
              onClick={e => { e.stopPropagation(); dismiss(it.id); }} />
          )}
        </Flash>
      ))}
    </div>
  );
}
