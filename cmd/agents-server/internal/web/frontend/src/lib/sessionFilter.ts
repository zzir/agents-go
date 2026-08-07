/* Client-side session search: case-insensitive substring match on the name.
   Lives in lib (not the component) so the node-environment vitest suite can
   cover it without pulling in Primer or CSS. */
export function filterSessionsByName<T extends { name: string }>(sessions: T[], query: string): T[] {
  const q = query.trim().toLowerCase();
  if (!q) return sessions;
  return sessions.filter(s => (s.name || '').toLowerCase().includes(q));
}
