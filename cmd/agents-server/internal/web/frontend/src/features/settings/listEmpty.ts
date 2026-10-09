// listEmpty words a scoped list's blank state by WHY it is blank (invariant 74);
// the noun travels along for LoadError's line.
export function listEmpty(opts: {
  // Plural noun as the list titles it ("agents", "MCP servers").
  noun: string;
  total: number;
  query: string;
  mine: boolean;
  // What the thing is, for a list with nothing in it yet.
  hint: string;
  addHint?: string;
}): { noun: string; empty: string; emptyHint?: string } {
  const { noun, total, query, mine, hint, addHint = '+ Add makes one.' } = opts;
  if (total === 0) return { noun, empty: `No ${noun} yet.`, emptyHint: `${hint} ${addHint}` };
  const q = query.trim();
  if (q) return { noun, empty: `No ${noun} match “${q}”.` };
  if (mine) return { noun, empty: `None of the ${noun} are yours.`, emptyHint: `Switch to All to see every member’s. ${addHint}` };
  return { noun, empty: `No ${noun} to show.` };
}
