import { useContext, useEffect, useId, useState, type ReactNode } from 'react';
import { FormDirtyContext, UnsavedContext } from '@/lib/unsaved';

/** Wraps an editor and reports whether it was edited (an input/change event
 * marks it; a button control such as a switch raises neither; unmount clears it)
 * to the registry and FormDirtyContext. A form mounted past Save passes `dirty`. */
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
