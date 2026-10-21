// A Traces lens entry: a run with the wake-up runs its tasks triggered chained
// under it, in insertion order — one exchange. rootOf routes any run in a
// chain to its root; a wake-up run whose parent is absent (retention) is a
// root of its own.
export interface TraceGroup { rootId: string; runIds: string[] }

export function groupTraceRuns(runIds: string[], runParents?: Record<string, string>): { groups: TraceGroup[]; rootOf: Record<string, string> } {
  const present = new Set(runIds);
  const children: Record<string, string[]> = {};
  const roots: string[] = [];
  for (const rid of runIds) {
    const parent = runParents?.[rid];
    if (parent && parent !== rid && present.has(parent)) {
      (children[parent] ||= []).push(rid);
    } else {
      roots.push(rid);
    }
  }
  const groups: TraceGroup[] = [];
  const rootOf: Record<string, string> = {};
  const seen = new Set<string>();
  for (const root of roots) {
    const ids: string[] = [];
    const visit = (rid: string) => {
      if (seen.has(rid)) return;
      seen.add(rid);
      rootOf[rid] = root;
      ids.push(rid);
      for (const c of children[rid] || []) visit(c);
    };
    visit(root);
    groups.push({ rootId: root, runIds: ids });
  }
  return { groups, rootOf };
}
