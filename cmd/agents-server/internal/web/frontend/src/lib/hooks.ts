import { useState, useEffect, useCallback, useRef, type DependencyList, type RefCallback } from 'react';
import { toast } from '@/lib/toast';

interface UseApiResult<T> {
  data: T | null;
  loading: boolean;
  error: string | null;
  reload: () => Promise<void>;
}

export function useApi<T>(fetcher: () => Promise<T>, deps: DependencyList = []): UseApiResult<T> {
  const [data, setData] = useState<T | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const reload = useCallback(async () => {
    setLoading(true);
    try {
      const result = await fetcher();
      setData(result);
      setError(null);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setLoading(false);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);

  useEffect(() => { reload(); }, [reload]);

  return { data, loading, error, reload };
}

type CrudId = string | number;

interface CrudResource<F> {
  list: () => Promise<unknown>;
  create: (data: F) => Promise<unknown>;
  update: (id: CrudId, data: F) => Promise<unknown>;
  delete: (id: CrudId) => Promise<unknown>;
}

interface UseCrudResult<T, F> {
  items: T[];
  loading: boolean;
  reload: () => Promise<void>;
  adding: boolean;
  editing: T | null;
  startAdd: () => void;
  startEdit: (item: T) => void;
  cancel: () => void;
  save: (form: F) => Promise<void>;
  remove: (id: CrudId) => Promise<void>;
}

/**
 * The list + add/edit/delete state machine every settings panel needs. Wraps a
 * CRUD `api.*` resource: tracks the adding/editing toggle, reloads after a
 * write, and routes every mutation failure through `toast.error` — so panels
 * don't each re-implement (and forget) error handling. Special per-panel
 * actions (OAuth connect, sandbox exec, …) stay in the panel and use `reload`.
 */
export function useCrud<T extends { id: CrudId }, F = Partial<T>>(
  resource: CrudResource<F>,
): UseCrudResult<T, F> {
  const { data, loading, reload } = useApi<T[]>(() => resource.list() as Promise<T[]>);
  const [adding, setAdding] = useState(false);
  const [editing, setEditing] = useState<T | null>(null);

  const startAdd = useCallback(() => { setEditing(null); setAdding(true); }, []);
  const startEdit = useCallback((item: T) => { setAdding(false); setEditing(item); }, []);
  const cancel = useCallback(() => { setAdding(false); setEditing(null); }, []);

  const save = useCallback(async (form: F) => {
    try {
      if (editing) await resource.update(editing.id, form);
      else await resource.create(form);
      setAdding(false);
      setEditing(null);
      await reload();
    } catch (e) {
      toast.error((e as Error).message);
    }
  }, [editing, resource, reload]);

  const remove = useCallback(async (id: CrudId) => {
    try {
      await resource.delete(id);
      await reload();
    } catch (e) {
      toast.error((e as Error).message);
    }
  }, [resource, reload]);

  return { items: data ?? [], loading, reload, adding, editing, startAdd, startEdit, cancel, save, remove };
}

interface ScrollAnchor {
  ref: RefCallback<HTMLElement>;
  isSticky: boolean;
  scrollToBottom: () => void;
}

export function useScrollToBottom(dep: unknown, resetDep: unknown): ScrollAnchor {
  const elRef = useRef<HTMLElement | null>(null);
  const handlerRef = useRef<(() => void) | null>(null);
  const stick = useRef(true);
  const [isSticky, setIsSticky] = useState(true);

  const updateSticky = useCallback((val: boolean) => {
    if (stick.current !== val) {
      stick.current = val;
      setIsSticky(val);
    }
  }, []);

  const ref: RefCallback<HTMLElement> = useCallback((node: HTMLElement | null) => {
    if (elRef.current && handlerRef.current) {
      elRef.current.removeEventListener('scroll', handlerRef.current);
    }
    elRef.current = node;
    if (node) {
      const handler = () => {
        const dist = node.scrollHeight - node.scrollTop - node.clientHeight;
        updateSticky(dist < 80);
      };
      handlerRef.current = handler;
      node.addEventListener('scroll', handler, { passive: true });
      node.scrollTop = node.scrollHeight;
    }
  }, [updateSticky]);

  useEffect(() => {
    const onSelect = () => {
      const sel = document.getSelection();
      const el = elRef.current;
      if (sel && !sel.isCollapsed && el?.contains(sel.anchorNode)) {
        const dist = el.scrollHeight - el.scrollTop - el.clientHeight;
        if (dist >= 80) updateSticky(false);
      }
    };
    document.addEventListener('selectionchange', onSelect);
    return () => document.removeEventListener('selectionchange', onSelect);
  }, [updateSticky]);

  useEffect(() => { updateSticky(true); }, [resetDep, updateSticky]);

  useEffect(() => {
    if (elRef.current && stick.current) {
      elRef.current.scrollTop = elRef.current.scrollHeight;
    }
  }, [dep, resetDep]);

  const scrollToBottom = useCallback(() => {
    if (elRef.current) {
      elRef.current.scrollTop = elRef.current.scrollHeight;
      updateSticky(true);
    }
  }, [updateSticky]);

  return { ref, isSticky, scrollToBottom };
}
