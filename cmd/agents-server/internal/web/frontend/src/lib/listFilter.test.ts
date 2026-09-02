import { describe, expect, it } from 'vitest';
import { filterRows } from '@/lib/listFilter';

const rows = [
  { id: 1, name: 'alpha', owner_id: 'me' },
  { id: 2, name: 'beta', owner_id: 'them' },
  { id: 3, name: 'Alphabet', owner_id: undefined },
];
const text = (r: { name: string }) => r.name;

describe('filterRows', () => {
  it('keeps every row with no filter', () => {
    expect(filterRows(rows, { mine: false, query: '' }, text)).toHaveLength(3);
  });
  it('mine keeps only the caller\'s rows, an authorless row included in neither', () => {
    expect(filterRows(rows, { mine: true, meId: 'me', query: '' }, text).map(r => r.id)).toEqual([1]);
  });
  it('matches the query case-insensitively', () => {
    expect(filterRows(rows, { mine: false, query: 'ALPHA' }, text).map(r => r.id)).toEqual([1, 3]);
  });
});
