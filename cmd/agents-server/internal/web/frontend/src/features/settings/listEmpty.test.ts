import { describe, it, expect } from 'vitest';
import { listEmpty } from '@/features/settings/listEmpty';

const base = { noun: 'agents', hint: 'An agent is a model with instructions.' };

describe('listEmpty', () => {
  it('says nothing exists yet, and how to make one', () => {
    const r = listEmpty({ ...base, total: 0, query: '', mine: true });
    expect(r.empty).toBe('No agents yet.');
    expect(r.emptyHint).toBe('An agent is a model with instructions. + Add makes one.');
    expect(listEmpty({ ...base, total: 0, query: '', mine: false, addHint: 'Import brings some.' }).emptyHint).toContain('Import brings some.');
  });

  // Rows exist but the search matched none: the search is the reason, whatever
  // the owner filter says.
  it('names the search that matched nothing', () => {
    const r = listEmpty({ ...base, total: 3, query: ' gpt ', mine: true });
    expect(r.empty).toBe('No agents match “gpt”.');
    expect(r.emptyHint).toBeUndefined();
  });

  it('says the Mine filter hides every row, and where the rest are', () => {
    const r = listEmpty({ ...base, total: 3, query: '', mine: true });
    expect(r.empty).toBe('None of the agents are yours.');
    expect(r.emptyHint).toContain('Switch to All');
  });

  it('has a fallback for an emptied list under All', () => {
    expect(listEmpty({ ...base, total: 3, query: '', mine: false }).empty).toBe('No agents to show.');
  });
});
