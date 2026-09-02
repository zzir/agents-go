import type { ScopedRow } from '@/lib/access';

export interface ListFilter {
  mine: boolean;
  meId?: string;
  query: string;
}

// filterRows applies a list's toolbar: the owner filter, then a
// case-insensitive substring match over the text a row is searched by.
export function filterRows<T extends ScopedRow>(rows: T[], f: ListFilter, text: (row: T) => string): T[] {
  const q = f.query.trim().toLowerCase();
  return rows.filter(r =>
    (!f.mine || (!!f.meId && r.owner_id === f.meId))
    && (!q || text(r).toLowerCase().includes(q)));
}
