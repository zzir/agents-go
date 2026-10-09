// The sidebar reads the session list a page at a time (GET /sessions?limit=):
// pinned sessions ride with the first page, and scrolling to the end asks for a
// longer prefix, not the next page, so a refresh of what is shown is one request.

export const SESSION_PAGE = 100;

// SESSION_LISTS matches every cached session listing — the sidebar's pages
// and the whole list other panels read — for one invalidation.
export const SESSION_LISTS = /^sessions(:|$)/;

// sessionListKey names the cache entry of one page request.
export function sessionListKey(limit: number, q: string): string {
  return `sessions:list:${limit}:${q}`;
}

// hasMoreSessions: a page whose unpinned rows fill the limit may have more
// behind it; one that does not is the end.
export function hasMoreSessions(rows: Array<{ pinned?: boolean }>, limit: number): boolean {
  let unpinned = 0;
  for (const r of rows) if (!r.pinned) unpinned++;
  return unpinned >= limit;
}
