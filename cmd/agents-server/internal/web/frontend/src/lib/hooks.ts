import { useState, useEffect, useCallback, useRef, type DependencyList, type RefCallback, type PointerEvent, type KeyboardEvent } from 'react';
import { useConfirm } from '@primer/react';
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
// A collapsible pane's hysteresis, in pixels inside `min`: an edge dragged
// past the first snaps to the rail, and only one dragged back past the second
// snaps out again.
const COLLAPSE_GAP = 60;
const EXPAND_GAP = 40;
// How long `snapping` stays on after a snap — longer than the CSS transition.
const SNAP_MS = 200;

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

function readStoredFlag(key: string): boolean {
  try {
    return localStorage.getItem(key) === '1';
  } catch {
    return false;
  }
}

function saveStoredFlag(key: string, on: boolean): void {
  try {
    localStorage.setItem(key, on ? '1' : '0');
  } catch {
    // As above.
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
  /** The width the pane snaps to when its edge is dragged well inside `min`
   *  (the sidebar's icon rail). Absent, the drag stops at `min`. */
  collapsedWidth?: number;
}

interface ResizablePane {
  /** The expanded width, kept while `collapsed` so expanding returns to it. */
  width: number;
  collapsed: boolean;
  /** True for a moment after a snap between the two shapes: the pane animates
   *  its width then, and never while it tracks the pointer. */
  snapping: boolean;
  /** True while a pointer drag is in flight — drives the handle's accent
   *  dragging visual (Primer PageLayout.DragHandle parity). */
  dragging: boolean;
  expand: () => void;
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
 * drag-handle element; apply `width` to the pane itself. With `collapsedWidth`
 * the pane also has a collapsed shape (invariant 68): a drag well inside `min`
 * snaps to it; a drag back out, `expand`, a widening arrow key or a double
 * click snaps out.
 */
export function useResizablePane({ storageKey, min, max, defaultWidth, edge, collapsedWidth }: UseResizablePaneOptions): ResizablePane {
  const collapsible = collapsedWidth !== undefined;
  const collapsedKey = storageKey + 'Collapsed';
  const [width, setWidth] = useState(() => clampPaneWidth(readStoredPaneWidth(storageKey, defaultWidth), min, max));
  const [collapsed, setCollapsed] = useState(() => collapsible && readStoredFlag(collapsedKey));
  const [snapping, setSnapping] = useState(false);
  const [dragging, setDragging] = useState(false);
  // The handlers read the two states through refs, set eagerly so a burst of
  // events between renders sees its own changes.
  const widthRef = useRef(width);
  widthRef.current = width;
  const collapsedRef = useRef(collapsed);
  collapsedRef.current = collapsed;
  const snapTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const dragStartXRef = useRef(0);
  const dragStartWidthRef = useRef(0);
  const sign = edge === 'left' ? 1 : -1;

  useEffect(() => () => { if (snapTimer.current) clearTimeout(snapTimer.current); }, []);

  const snapTo = useCallback((to: boolean) => {
    collapsedRef.current = to;
    setCollapsed(to);
    saveStoredFlag(collapsedKey, to);
    setSnapping(true);
    if (snapTimer.current) clearTimeout(snapTimer.current);
    snapTimer.current = setTimeout(() => { snapTimer.current = null; setSnapping(false); }, SNAP_MS);
  }, [collapsedKey]);

  // A width that follows the pointer or a key is never animated: it cuts a
  // snap in progress short.
  const track = useCallback((next: number) => {
    widthRef.current = next;
    setWidth(next);
    if (snapTimer.current) {
      clearTimeout(snapTimer.current);
      snapTimer.current = null;
      setSnapping(false);
    }
  }, []);

  const onPointerDown = useCallback((e: PointerEvent<HTMLDivElement>) => {
    if (e.button !== 0) return;
    e.preventDefault();
    try {
      e.currentTarget.setPointerCapture(e.pointerId);
    } catch {
      // Pointer capture is a nice-to-have; ignore if unsupported/unavailable.
    }
    dragStartXRef.current = e.clientX;
    dragStartWidthRef.current = collapsedRef.current && collapsedWidth !== undefined ? collapsedWidth : widthRef.current;
    setDragging(true);
  }, [collapsedWidth]);

  const onPointerMove = useCallback((e: PointerEvent<HTMLDivElement>) => {
    if (!e.currentTarget.hasPointerCapture(e.pointerId)) return;
    e.preventDefault();
    // Where the edge would be if it simply followed the pointer.
    const target = dragStartWidthRef.current + (e.clientX - dragStartXRef.current) * sign;
    if (collapsible) {
      if (collapsedRef.current) {
        if (target > min - EXPAND_GAP) {
          snapTo(false);
          widthRef.current = clampPaneWidth(target, min, max);
          setWidth(widthRef.current);
        }
        return;
      }
      if (target < min - COLLAPSE_GAP) {
        snapTo(true);
        return;
      }
    }
    const next = clampPaneWidth(target, min, max);
    if (next !== widthRef.current) track(next);
  }, [collapsible, min, max, sign, snapTo, track]);

  const onPointerUp = useCallback((e: PointerEvent<HTMLDivElement>) => {
    setDragging(false);
    if (!e.currentTarget.hasPointerCapture(e.pointerId)) return;
    savePaneWidth(storageKey, widthRef.current);
  }, [storageKey]);

  const onKeyDown = useCallback((e: KeyboardEvent<HTMLDivElement>) => {
    if (e.key !== 'ArrowLeft' && e.key !== 'ArrowRight') return;
    e.preventDefault();
    // Positive widens the pane, whichever side it is docked to.
    const step = (e.key === 'ArrowLeft' ? -1 : 1) * RESIZE_ARROW_KEY_STEP * sign;
    if (collapsible) {
      if (collapsedRef.current) {
        if (step > 0) snapTo(false);
        return;
      }
      if (step < 0 && widthRef.current <= min) {
        snapTo(true);
        return;
      }
    }
    const next = clampPaneWidth(widthRef.current + step, min, max);
    if (next !== widthRef.current) {
      track(next);
      savePaneWidth(storageKey, next);
    }
  }, [collapsible, min, max, sign, storageKey, snapTo, track]);

  const onDoubleClick = useCallback(() => {
    if (collapsedRef.current) snapTo(false);
    widthRef.current = defaultWidth;
    setWidth(defaultWidth);
    savePaneWidth(storageKey, defaultWidth);
  }, [defaultWidth, storageKey, snapTo]);

  const expand = useCallback(() => {
    if (collapsedRef.current) snapTo(false);
  }, [snapTo]);

  return { width, collapsed, snapping, dragging, expand, handleProps: { onPointerDown, onPointerMove, onPointerUp, onLostPointerCapture: onPointerUp, onKeyDown, onDoubleClick } };
}

interface UseApiResult<T> {
  data: T | null;
  loading: boolean;
  error: string | null;
  reload: (opts?: { throwOnError?: boolean }) => Promise<void>;
  // mutateData updates the data without a refetch — with a key, for every
  // consumer of it — so a mutation shows at once and reconciles later.
  mutateData: (fn: (prev: T | null) => T | null) => void;
}

// The shared response cache behind keyed useApi calls: one entry per key,
// served to every mount, revalidated once it is older than CACHE_TTL_MS, and
// dropped by invalidate(). A fetch in flight is shared, so eight panels asking
// for the agent list at once make one request.
const CACHE_TTL_MS = 30_000;

interface CacheEntry {
  data: unknown;
  at: number;
  inflight: Promise<unknown> | null;
}

type CacheListener = (ev: { kind: 'data'; data: unknown } | { kind: 'invalidate' }) => void;

const cache = new Map<string, CacheEntry>();
const listeners = new Map<string, Set<CacheListener>>();

function subscribe(key: string, fn: CacheListener): () => void {
  let set = listeners.get(key);
  if (!set) { set = new Set(); listeners.set(key, set); }
  set.add(fn);
  return () => { set.delete(fn); if (set.size === 0) listeners.delete(key); };
}

function notify(key: string, ev: Parameters<CacheListener>[0]): void {
  for (const fn of listeners.get(key) || []) fn(ev);
}

// fetchShared runs the fetcher once per key at a time: a second caller joins
// the request in flight, and the answer lands in the cache before it resolves.
function fetchShared<T>(key: string, fetcher: () => Promise<T>): Promise<T> {
  const cur = cache.get(key);
  if (cur?.inflight) return cur.inflight as Promise<T>;
  const p = fetcher().then(result => {
    cache.set(key, { data: result, at: Date.now(), inflight: null });
    notify(key, { kind: 'data', data: result });
    return result;
  }, err => {
    const e = cache.get(key);
    if (e?.inflight === p) e.inflight = null;
    throw err;
  });
  cache.set(key, { data: cur?.data, at: cur?.at ?? 0, inflight: p });
  return p;
}

/** Drops every cached response whose key matches, and has each mounted
 * consumer of it refetch. A mutation that changes a list elsewhere (a save
 * outside useCrud, a scope flip) calls this with the list's key. */
export function invalidate(key: string | RegExp): void {
  const matches = (k: string) => (typeof key === 'string' ? k === key : key.test(k));
  const keys = new Set([...cache.keys(), ...listeners.keys()].filter(matches));
  for (const k of keys) {
    cache.delete(k);
    notify(k, { kind: 'invalidate' });
  }
}

/** Fetches once on mount and again when `deps` change. With a `key`, the
 * response is shared through the cache above: a mount finds the last answer
 * at once (revalidating it in the background past the TTL), a reload anywhere
 * reaches every consumer, and invalidate(key) refetches them all. Pick a key
 * that changes with `deps` (put the id in it). */
export function useApi<T>(fetcher: () => Promise<T>, deps: DependencyList = [], key?: string): UseApiResult<T> {
  const cached = key ? cache.get(key) : undefined;
  const [data, setData] = useState<T | null>(cached && cached.at > 0 ? cached.data as T : null);
  const [loading, setLoading] = useState(!(cached && cached.at > 0));
  const [error, setError] = useState<string | null>(null);
  // Monotonic request id: only the newest reload/mutateData may write data,
  // error and loading — a slow earlier reload can't overwrite a newer result or
  // a just-applied optimistic update.
  const genRef = useRef(0);

  const reload = useCallback(async (opts?: { throwOnError?: boolean }) => {
    const gen = ++genRef.current;
    setLoading(true);
    try {
      const result = await (key ? fetchShared(key, fetcher) : fetcher());
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
  }, [...deps, key]);

  const mutateData = useCallback((fn: (prev: T | null) => T | null) => {
    // Bump the generation so any in-flight reload's result is discarded — the
    // optimistic update is the source of truth until a newer reload lands.
    genRef.current++;
    if (!key) { setData(prev => fn(prev)); return; }
    // Keyed: the cache is the truth, and the notify reaches this consumer
    // and every other mount of the key alike.
    const e = cache.get(key);
    const next = fn((e?.data as T | undefined) ?? null);
    cache.set(key, { data: next, at: e?.at ?? Date.now(), inflight: e?.inflight ?? null });
    notify(key, { kind: 'data', data: next });
  }, [key]);

  useEffect(() => {
    if (!key) { reload(); return; }
    const entry = cache.get(key);
    if (entry && entry.at > 0) {
      setData(entry.data as T);
      setError(null);
      setLoading(false);
      if (Date.now() - entry.at <= CACHE_TTL_MS) return;
    }
    reload();
  }, [reload, key]);

  useEffect(() => {
    if (!key) return;
    return subscribe(key, ev => {
      if (ev.kind === 'invalidate') { reload(); return; }
      setData(ev.data as T);
      setError(null);
      setLoading(false);
    });
  }, [key, reload]);

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
  error: string | null;
  reload: () => Promise<void>;
  adding: boolean;
  editing: T | null;
  // A save in flight; the form's Save button waits on it.
  saving: boolean;
  startAdd: () => void;
  startEdit: (item: T) => void;
  cancel: () => void;
  // True once the write landed and the list reloaded; false on an API error.
  save: (form: F) => Promise<boolean>;
  remove: (id: CrudId, label?: string) => Promise<boolean>;
}

/**
 * The list + add/edit/delete state machine every settings panel needs. Wraps a
 * CRUD `api.*` resource: tracks the adding/editing toggle, reloads after a
 * write, and routes every mutation failure through `toast.error` — so panels
 * don't each re-implement (and forget) error handling. Special per-panel
 * actions (OAuth connect, sandbox exec, …) stay in the panel and use `reload`.
 * `key` is the list's cache key (see useApi): every write reloads through it,
 * so a picker elsewhere holding the same list sees the change.
 */
export function useCrud<T extends { id: CrudId }, F = Partial<T>>(
  resource: CrudResource<F>,
  key?: string,
): UseCrudResult<T, F> {
  const confirmDialog = useConfirm();
  const { data, loading, error, reload } = useApi<T[]>(() => resource.list() as Promise<T[]>, [], key);
  const [adding, setAdding] = useState(false);
  const [editing, setEditing] = useState<T | null>(null);
  const [saving, setSaving] = useState(false);
  const savingRef = useRef(false);

  const startAdd = useCallback(() => { setEditing(null); setAdding(true); }, []);
  const startEdit = useCallback((item: T) => { setAdding(false); setEditing(item); }, []);
  const cancel = useCallback(() => { setAdding(false); setEditing(null); }, []);

  const save = useCallback(async (form: F) => {
    // The ref is the same-tick guard: two clicks can land before a state
    // write renders.
    if (savingRef.current) return false;
    savingRef.current = true;
    setSaving(true);
    try {
      if (editing) await resource.update(editing.id, form);
      else await resource.create(form);
      setAdding(false);
      setEditing(null);
      await reload();
      return true;
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
      return false;
    } finally {
      savingRef.current = false;
      setSaving(false);
    }
  }, [editing, resource, reload]);

  // Every delete confirms here, so a new panel cannot forget the guard.
  // Returns whether the row was deleted (false on decline or API error).
  const remove = useCallback(async (id: CrudId, label?: string) => {
    const ok = await confirmDialog({
      title: label ? `Delete “${label}”?` : 'Delete this item?',
      content: 'This cannot be undone.',
      confirmButtonContent: 'Delete',
      confirmButtonType: 'danger',
    });
    if (!ok) return false;
    try {
      await resource.delete(id);
      await reload();
      return true;
    } catch (e) {
      toast.error((e as Error).message);
      return false;
    }
  }, [confirmDialog, resource, reload]);

  return { items: data ?? [], loading, error, reload, adding, editing, saving, startAdd, startEdit, cancel, save, remove };
}

/** Copy-to-clipboard with the 1.5s "Copied" flip every copy button shows.
 * `copied` holds the key passed to `copy` (default 'default') until the flip
 * ends, null otherwise — so one hook serves multi-target boxes too. A denied
 * clipboard permission reports via toast instead of failing silently; the
 * promise says whether the copy happened, for a caller that flips its own
 * button (markup outside React). */
export function useCopy(): { copied: string | null; copy: (text: string, key?: string) => Promise<boolean> } {
  const [copied, setCopied] = useState<string | null>(null);
  const timer = useRef<number | undefined>(undefined);
  useEffect(() => () => window.clearTimeout(timer.current), []);
  const copy = useCallback(async (text: string, key = 'default') => {
    // No clipboard API on an insecure (plain-http LAN) origin.
    if (!navigator.clipboard) { toast.error('Could not copy — select it and copy by hand'); return false; }
    try {
      await navigator.clipboard.writeText(text);
    } catch {
      toast.error('Could not copy — select it and copy by hand');
      return false;
    }
    setCopied(key);
    window.clearTimeout(timer.current);
    timer.current = window.setTimeout(() => setCopied(null), 1500);
    return true;
  }, []);
  return { copied, copy };
}

/** Ticks once a second while `live`; returns the current ms timestamp for duration labels. */
export function useNowTicker(live: boolean): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!live) return;
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, [live]);
  return now;
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
