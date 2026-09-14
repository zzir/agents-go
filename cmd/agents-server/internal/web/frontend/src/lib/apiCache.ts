// The shared response cache behind keyed useApi calls: one entry per key,
// served to every mount, revalidated once it is older than CACHE_TTL_MS, and
// dropped by invalidate(). A fetch in flight is shared, so eight panels asking
// for the agent list at once make one request. Free of React and Primer so
// the socket layer can invalidate a list without pulling either in.
export const CACHE_TTL_MS = 30_000;

interface CacheEntry {
  data: unknown;
  at: number;
  inflight: Promise<unknown> | null;
}

export type CacheEvent = { kind: 'data'; data: unknown } | { kind: 'invalidate' };
type CacheListener = (ev: CacheEvent) => void;

const cache = new Map<string, CacheEntry>();
const listeners = new Map<string, Set<CacheListener>>();

export function getCached(key: string): CacheEntry | undefined {
  return cache.get(key);
}

export function setCached(key: string, entry: CacheEntry): void {
  cache.set(key, entry);
}

export function subscribe(key: string, fn: CacheListener): () => void {
  let set = listeners.get(key);
  if (!set) { set = new Set(); listeners.set(key, set); }
  set.add(fn);
  return () => { set.delete(fn); if (set.size === 0) listeners.delete(key); };
}

export function notify(key: string, ev: CacheEvent): void {
  for (const fn of listeners.get(key) || []) fn(ev);
}

// fetchShared runs the fetcher once per key at a time: a second caller joins
// the request in flight, and the answer lands in the cache before it resolves.
export function fetchShared<T>(key: string, fetcher: () => Promise<T>): Promise<T> {
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
