import { useCallback, useEffect, useMemo, useRef } from 'react';
import { useConfirm } from '@primer/react';
import { DISCARD_PROMPT, type UnsavedRegistry } from '@/lib/unsaved';

/** The registry a dialog or page provides through UnsavedContext, and the close
 * that asks first while any form under it is dirty; leaving the page asks via
 * beforeunload (invariant 41). */
export function useUnsavedRegistry(): { registry: UnsavedRegistry; guardedClose: (close: () => void) => Promise<void> } {
  const dirtyForms = useRef(new Set<string>());
  const registry = useMemo<UnsavedRegistry>(() => ({
    set: (id, dirty) => { if (dirty) dirtyForms.current.add(id); else dirtyForms.current.delete(id); },
    any: () => dirtyForms.current.size > 0,
  }), []);
  const confirm = useConfirm();
  const guardedClose = useCallback(async (close: () => void) => {
    if (registry.any() && !(await confirm(DISCARD_PROMPT))) return;
    close();
  }, [registry, confirm]);
  useEffect(() => {
    const onBeforeUnload = (e: BeforeUnloadEvent) => {
      if (!registry.any()) return;
      e.preventDefault();
      // What older engines read instead of the cancelled default.
      e.returnValue = '';
    };
    window.addEventListener('beforeunload', onBeforeUnload);
    return () => window.removeEventListener('beforeunload', onBeforeUnload);
  }, [registry]);
  return { registry, guardedClose };
}
