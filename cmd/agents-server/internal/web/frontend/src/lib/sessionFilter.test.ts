import { describe, expect, it } from 'vitest';
import { filterSessionsByName } from './sessionFilter';

const sessions = [
  { name: 'New Session' },
  { name: 'Debug the run loop' },
  { name: 'session about compaction' },
];

describe('filterSessionsByName', () => {
  it('returns the input unchanged for an empty or whitespace query', () => {
    expect(filterSessionsByName(sessions, '')).toBe(sessions);
    expect(filterSessionsByName(sessions, '   ')).toBe(sessions);
  });

  it('matches case-insensitively on a substring, preserving order', () => {
    expect(filterSessionsByName(sessions, 'SESSION')).toEqual([
      { name: 'New Session' },
      { name: 'session about compaction' },
    ]);
    expect(filterSessionsByName(sessions, 'run loop')).toEqual([{ name: 'Debug the run loop' }]);
  });

  it('returns [] when nothing matches', () => {
    expect(filterSessionsByName(sessions, 'nonexistent')).toEqual([]);
  });

  it('tolerates a missing name', () => {
    expect(filterSessionsByName([{ name: undefined as unknown as string }], 'x')).toEqual([]);
  });
});
