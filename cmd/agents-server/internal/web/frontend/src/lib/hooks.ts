import { useState, useEffect, useCallback, useRef, type DependencyList, type RefCallback, type PointerEvent, type KeyboardEvent } from 'react';
import { toast } from '@/lib/toast';

const NARROW_QUERY = '(max-width: 767px)';

/** True below the app's mobile breakpoint (767px); tracks live via matchMedia. */
export function useNarrow(): boolean {
  const [narrow, setNarrow] = useState(() => window.matchMedia(NARROW_QUERY).matches);
  useEffect(() => {
    const mql = window.matchMedia(NARROW_QUERY);
    const handler = (e: MediaQueryListEvent) => setNarrow(e.matches);
    mql.addEventListener('change', handler);
    return () => mql.removeEventListener('change', handler);
  }, []);
  return narrow;
}

const RESIZE_ARROW_KEY_STEP = 10;

function clampPaneWidth(n: number, min: number, max: number): number {
  return Math.min(max, Math.max(min, n));
}

function readStoredPaneWidth(storageKey: string, fallback: number): number {
  try {
    const raw = localStorage.getItem(storageKey);
    if (raw === null) return fallback;
    const n = Math.round(Number(raw));
    return Number.isFinite(n) && n > 0 ? n : fallback;
  } catch {
    return fallback;
  }
}

function savePaneWidth(storageKey: string, width: number): void {
  try {
    localStorage.setItem(storageKey, String(Math.round(width)));
  } catch {
    // Ignore write errors (private browsing, quota exceeded, etc.)
  }
}

interface UseResizablePaneOptions {
  storageKey: string;
  min: number;
  max: number;
  defaultWidth: number;
  /** Which side of the viewport the pane is docked to — flips the drag sign:
   *  a 'left'-docked pane (sidebar) grows when its edge is dragged right, a
   *  'right'-docked one (a trace/detail drawer) grows when dragged left. */
  edge: 'left' | 'right';
}

interface ResizablePane {
  width: number;
  /** True while a pointer drag is in flight — drives the handle's accent
   *  dragging visual (Primer PageLayout.DragHandle parity). */
  dragging: boolean;
  handleProps: {
    onPointerDown: (e: PointerEvent<HTMLDivElement>) => void;
    onPointerMove: (e: PointerEvent<HTMLDivElement>) => void;
    onPointerUp: (e: PointerEvent<HTMLDivElement>) => void;
    onLostPointerCapture: (e: PointerEvent<HTMLDivElement>) => void;
    onKeyDown: (e: KeyboardEvent<HTMLDivElement>) => void;
    onDoubleClick: () => void;
  };
}

/**
 * Drag-to-resize behavior shared by the sidebar and any right-docked panel
 * (trace/detail drawers): pointer-drag width persisted per storageKey, with
 * arrow-key nudging and double-click-to-reset. Spread `handleProps` onto the
 * drag-handle element; apply `width` to the pane itself.
 */
export function useResizablePane({ storageKey, min, max, defaultWidth, edge }: UseResizablePaneOptions): ResizablePane {
  const [width, setWidth] = useState(() => clampPaneWidth(readStoredPaneWidth(storageKey, defaultWidth), min, max));
  const [dragging, setDragging] = useState(false);
  const widthRef = useRef(width);
  widthRef.current = width;
  const dragStartXRef = useRef(0);
  const dragStartWidthRef = useRef(0);
  const sign = edge === 'left' ? 1 : -1;

  const onPointerDown = useCallback((e: PointerEvent<HTMLDivElement>) => {
    if (e.button !== 0) return;
    e.preventDefault();
    try {
      e.currentTarget.setPointerCapture(e.pointerId);
    } catch {
      // Pointer capture is a nice-to-have; ignore if unsupported/unavailable.
    }
    dragStartXRef.current = e.clientX;
    dragStartWidthRef.current = widthRef.current;
    setDragging(true);
  }, []);

  const onPointerMove = useCallback((e: PointerEvent<HTMLDivElement>) => {
    if (!e.currentTarget.hasPointerCapture(e.pointerId)) return;
    e.preventDefault();
    const delta = (e.clientX - dragStartXRef.current) * sign;
    const next = clampPaneWidth(dragStartWidthRef.current + delta, min, max);
    if (next !== widthRef.current) setWidth(next);
  }, [min, max, sign]);

  const onPointerUp = useCallback((e: PointerEvent<HTMLDivElement>) => {
    setDragging(false);
    if (!e.currentTarget.hasPointerCapture(e.pointerId)) return;
    savePaneWidth(storageKey, widthRef.current);
  }, [storageKey]);

  const onKeyDown = useCallback((e: KeyboardEvent<HTMLDivElement>) => {
    if (e.key !== 'ArrowLeft' && e.key !== 'ArrowRight') return;
    e.preventDefault();
    const dir = e.key === 'ArrowLeft' ? -1 : 1;
    const next = clampPaneWidth(widthRef.current + dir * RESIZE_ARROW_KEY_STEP * sign, min, max);
    if (next !== widthRef.current) {
      setWidth(next);
      savePaneWidth(storageKey, next);
    }
  }, [min, max, sign, storageKey]);

  const onDoubleClick = useCallback(() => {
    setWidth(defaultWidth);
    savePaneWidth(storageKey, defaultWidth);
  }, [defaultWidth, storageKey]);

  return { width, dragging, handleProps: { onPointerDown, onPointerMove, onPointerUp, onLostPointerCapture: onPointerUp, onKeyDown, onDoubleClick } };
}

interface UseApiResult<T> {
  data: T | null;
  loading: boolean;
  error: string | null;
  reload: (opts?: { throwOnError?: boolean }) => Promise<void>;
  // mutateData optimistically updates the cached data without a refetch, so a
  // mutation can migrate the local view immediately and reconcile in the
  // background.
  mutateData: (fn: (prev: T | null) => T | null) => void;
}

export function useApi<T>(fetcher: () => Promise<T>, deps: DependencyList = []): UseApiResult<T> {
  const [data, setData] = useState<T | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  // Monotonic request id: only the newest reload/mutateData may write data,
  // error and loading — a slow earlier reload can't overwrite a newer result or
  // a just-applied optimistic update.
  const genRef = useRef(0);

  const reload = useCallback(async (opts?: { throwOnError?: boolean }) => {
    const gen = ++genRef.current;
    setLoading(true);
    try {
      const result = await fetcher();
      if (gen === genRef.current) {
        setData(result);
        setError(null);
      }
    } catch (e) {
      if (gen === genRef.current) setError((e as Error).message);
      // Auto-refreshes (useEffect, timers, event handlers) fire-and-forget and
      // must not reject; only a caller that opts in — a mutation awaiting the
      // refresh — gets the error propagated.
      if (opts?.throwOnError) throw e;
    } finally {
      if (gen === genRef.current) setLoading(false);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);

  const mutateData = useCallback((fn: (prev: T | null) => T | null) => {
    // Bump the generation so any in-flight reload's result is discarded — the
    // optimistic update is the source of truth until a newer reload lands.
    genRef.current++;
    setData(prev => fn(prev));
  }, []);

  useEffect(() => { reload(); }, [reload]);

  return { data, loading, error, reload, mutateData };
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
      // A 409 on an EDIT means the row changed under the form: resubmitting
      // the stale snapshot can only 409 again, and the list still holds the
      // stale row — so close the editor and reload, and the next Edit starts
      // from current data. An add's 409 (duplicate name) keeps the form: the
      // fix is changing the input, which is worth keeping.
      if (editing && (e as { status?: number }).status === 409) {
        setEditing(null);
        await reload();
      }
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

// PAGE_SIZE is the rows a hub list shows at once, whether it pages on the
// server (runs) or in the browser (usePage).
export const PAGE_SIZE = 25;

interface Page<T> {
  items: T[];
  index: number;
  count: number;
  setIndex: (i: number) => void;
}

/**
 * usePage slices a whole list into pages of `size`, for a Table.Pagination fed
 * `defaultPageIndex={index}`. The index is clamped to the last page rather than
 * reset, so a delete on the last page shows the page before it, not an empty
 * one. setIndex ignores the current index: the pagination echoes a changed
 * defaultPageIndex through onChange while it renders, and a state write from
 * there would be an update to another component mid-render.
 */
export function usePage<T>(all: T[], size: number): Page<T> {
  const [index, setIndex] = useState(0);
  const count = Math.max(1, Math.ceil(all.length / size));
  const cur = Math.min(index, count - 1);
  // A clamp sticks: a list that grows back must not jump to the old page.
  useEffect(() => { if (index !== cur) setIndex(cur); }, [index, cur]);
  return {
    items: all.slice(cur * size, (cur + 1) * size),
    index: cur,
    count,
    setIndex: i => { if (i !== cur) setIndex(i); },
  };
}

interface ScrollAnchor {
  ref: RefCallback<HTMLElement>;
  isSticky: boolean;
  scrollToBottom: () => void;
}

export function useScrollToBottom(dep: unknown, resetDep: unknown): ScrollAnchor {
  const elRef = useRef<HTMLElement | null>(null);
  const cleanupRef = useRef<(() => void) | null>(null);
  const stick = useRef(true);
  const [isSticky, setIsSticky] = useState(true);

  const updateSticky = useCallback((val: boolean) => {
    if (!val) {
      const el = elRef.current;
      if (el && el.scrollHeight <= el.clientHeight) return;
    }
    if (stick.current !== val) {
      stick.current = val;
      setIsSticky(val);
    }
  }, []);

  // Timestamps of the last "stop following" intents. Both veto re-sticking
  // only while recent (350ms) — a standing state would deadlock against the
  // pin-to-bottom writes, a one-shot would lose to their trailing scroll
  // events, so recency is the discriminator.
  //  - lastSelChange: an actively changing selection (mid-drag). A static
  //    leftover selection must NOT veto — it survives the follow (morphdom
  //    keeps its nodes alive), and scrolling back down means "follow again".
  //  - lastUpIntent: an upward wheel/drag. While pinned, each delta rewrites
  //    scrollTop, so upward wheel motion barely moves the position and the
  //    dist<80 threshold takes several fighting frames to cross — the intent
  //    must win instantly, not by out-scrolling the pin.
  const lastSelChange = useRef(0);
  const lastUpIntent = useRef(0);
  const selectionInside = useCallback(() => {
    const sel = document.getSelection();
    return !!(sel && !sel.isCollapsed && elRef.current?.contains(sel.anchorNode));
  }, []);

  const ref: RefCallback<HTMLElement> = useCallback((node: HTMLElement | null) => {
    cleanupRef.current?.();
    cleanupRef.current = null;
    elRef.current = node;
    if (node) {
      // Trackpads fire scroll events well above frame rate; coalesce the
      // layout reads (scrollHeight/scrollTop) to one per frame.
      let rafId = 0;
      let prevTop = node.scrollTop;
      let prevDist = 0;
      const onScroll = () => {
        if (rafId) return;
        rafId = requestAnimationFrame(() => {
          rafId = 0;
          const dist = node.scrollHeight - node.scrollTop - node.clientHeight;
          // Position moved away from the bottom: upward scrollbar drag or
          // touch scroll (wheel is caught below, before position even moves).
          // The dist guard keeps content shrinkage — which clamps scrollTop
          // but leaves dist at 0 — from reading as user intent.
          const movedUp = node.scrollTop < prevTop - 1 && dist > prevDist + 1;
          prevTop = node.scrollTop;
          prevDist = dist;
          const now = performance.now();
          if (movedUp) {
            lastUpIntent.current = now;
            updateSticky(false);
          } else if (dist >= 80) {
            updateSticky(false);
          } else if (now - lastSelChange.current > 350 && now - lastUpIntent.current > 350) {
            // At the bottom with no recent stop-following intent: (re)stick.
            updateSticky(true);
          }
          // At the bottom but vetoed (mid-selection / just wheeled up): leave
          // the state as is — the veto blocks RE-sticking after an unstick,
          // it must not force an unstick while still pinned (that surfaced a
          // "Jump to latest" button with nothing below to jump to).
        });
      };
      const onWheel = (e: WheelEvent) => {
        if (e.ctrlKey) return; // pinch-zoom, not a scroll
        if (e.deltaY < 0) {
          lastUpIntent.current = performance.now();
          updateSticky(false);
        } else if (e.deltaY > 0) {
          lastUpIntent.current = 0; // wheeling down: let dist<80 re-stick at once
        }
      };
      node.addEventListener('scroll', onScroll, { passive: true });
      node.addEventListener('wheel', onWheel, { passive: true });
      node.scrollTop = node.scrollHeight;
      cleanupRef.current = () => {
        if (rafId) cancelAnimationFrame(rafId);
        node.removeEventListener('scroll', onScroll);
        node.removeEventListener('wheel', onWheel);
      };
    }
  }, [updateSticky]);

  useEffect(() => {
    // Making or growing a selection in the log suspends bottom-following even
    // at the bottom: auto-scroll would move the content under the cursor
    // mid-drag. Only selection *changes* unstick — a static leftover
    // selection doesn't keep re-unsticking, so the scroll handler above can
    // win once the user scrolls back down.
    const onSelect = () => {
      const el = elRef.current;
      if (!el) return;
      if (selectionInside()) {
        // Record the intent but do NOT unstick yet: with nothing arriving,
        // selecting at the bottom must not surface a "Jump to latest" button
        // for content that doesn't exist. The pin effect below unsticks
        // lazily, on the first content growth during an active selection.
        lastSelChange.current = performance.now();
      } else if (!stick.current) {
        // Selection cleared while still at the bottom: resume following.
        const dist = el.scrollHeight - el.scrollTop - el.clientHeight;
        if (dist < 80) updateSticky(true);
      }
    };
    document.addEventListener('selectionchange', onSelect);
    return () => document.removeEventListener('selectionchange', onSelect);
  }, [updateSticky, selectionInside]);

  useEffect(() => { updateSticky(true); }, [resetDep, updateSticky]);

  useEffect(() => {
    const el = elRef.current;
    if (!el || !stick.current) return;
    // Content arrived while a selection is actively changing (mid-drag):
    // following would move the text under the cursor, so hand over to the
    // unstuck state — the button appears now, when there genuinely is newer
    // content below. A static leftover selection doesn't veto (recency
    // window), matching the scroll handler's discriminator.
    if (performance.now() - lastSelChange.current < 350) {
      updateSticky(false);
      return;
    }
    el.scrollTop = el.scrollHeight;
  }, [dep, resetDep, updateSticky]);

  const scrollToBottom = useCallback(() => {
    if (elRef.current) {
      // Explicit "follow again" click: clear both re-stick vetoes so the
      // smooth scroll's own trailing events can't leave the view unstuck.
      lastUpIntent.current = 0;
      lastSelChange.current = 0;
      elRef.current.scrollTo({ top: elRef.current.scrollHeight, behavior: 'smooth' });
      updateSticky(true);
    }
  }, [updateSticky]);

  return { ref, isSticky, scrollToBottom };
}
