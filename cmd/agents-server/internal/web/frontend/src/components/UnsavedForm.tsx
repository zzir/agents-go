import { useContext, useEffect, useId, useState, type ReactNode } from 'react';
import { FormDirtyContext, UnsavedContext } from '@/lib/unsaved';

/** Wraps an editor and tracks whether it was edited: an input or change event
 * inside marks it, and the wrapper going away (the form closed on save or
 * cancel) clears it. The flag reaches the enclosing dialog's registry and the
 * form's own Cancel (FormDirtyContext). A control that is a button (a switch,
 * a segment) raises neither event and goes unguarded. */
export function UnsavedForm({ children, className }: { children: ReactNode; className?: string }) {
  const id = useId();
  const registry = useContext(UnsavedContext);
  const [dirty, setDirty] = useState(false);
  useEffect(() => { registry?.set(id, dirty); }, [registry, id, dirty]);
  useEffect(() => () => registry?.set(id, false), [registry, id]);
  const mark = () => { if (!dirty) setDirty(true); };
  return (
    <FormDirtyContext value={dirty}>
      <div className={className} onInput={mark} onChange={mark}>{children}</div>
    </FormDirtyContext>
  );
}
