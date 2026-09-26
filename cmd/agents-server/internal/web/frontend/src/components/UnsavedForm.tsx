import { useContext, useEffect, useId, useState, type ReactNode } from 'react';
import { FormDirtyContext, UnsavedContext } from '@/lib/unsaved';

/** Wraps an editor and tracks whether it was edited: an input or change event
 * inside marks it, and the wrapper going away (the form closed on save or
 * cancel) clears it. The flag reaches the enclosing registry and the form's
 * own Cancel (FormDirtyContext). A control that is a button (a switch, a
 * segment) raises neither event and goes unguarded. A form that stays mounted
 * after its Save (a settings row) knows its own state better — its draft
 * against the stored value — and passes it as `dirty` instead. */
export function UnsavedForm({ children, className, dirty: controlled }: { children: ReactNode; className?: string; dirty?: boolean }) {
  const id = useId();
  const registry = useContext(UnsavedContext);
  const [edited, setEdited] = useState(false);
  const dirty = controlled ?? edited;
  useEffect(() => { registry?.set(id, dirty); }, [registry, id, dirty]);
  useEffect(() => () => registry?.set(id, false), [registry, id]);
  const mark = () => { if (!edited) setEdited(true); };
  return (
    <FormDirtyContext value={dirty}>
      <div className={className} onInput={mark} onChange={mark}>{children}</div>
    </FormDirtyContext>
  );
}
