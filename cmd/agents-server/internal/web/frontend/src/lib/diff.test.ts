import { describe, it, expect } from 'vitest';
import { diffLines } from '@/lib/diff';

describe('diffLines', () => {
  it('identical inputs are all same', () => {
    expect(diffLines('a\nb', 'a\nb')).toEqual([
      { type: 'same', text: 'a' },
      { type: 'same', text: 'b' },
    ]);
  });

  it('a changed middle line is a del/add pair between same lines', () => {
    const d = diffLines('a\nx\nc', 'a\ny\nc');
    expect(d).toEqual([
      { type: 'same', text: 'a' },
      { type: 'del', text: 'x' },
      { type: 'add', text: 'y' },
      { type: 'same', text: 'c' },
    ]);
  });

  it('pure insertion and pure deletion', () => {
    expect(diffLines('a\nc', 'a\nb\nc')).toEqual([
      { type: 'same', text: 'a' },
      { type: 'add', text: 'b' },
      { type: 'same', text: 'c' },
    ]);
    expect(diffLines('a\nb\nc', 'a\nc')).toEqual([
      { type: 'same', text: 'a' },
      { type: 'del', text: 'b' },
      { type: 'same', text: 'c' },
    ]);
  });

  it('empty sides', () => {
    expect(diffLines('', '')).toEqual([]);
    expect(diffLines('', 'a')).toEqual([{ type: 'add', text: 'a' }]);
    expect(diffLines('a', '')).toEqual([{ type: 'del', text: 'a' }]);
  });

  it('the LCS keeps the longest common subsequence, not just anchors', () => {
    const d = diffLines('a\nb\nc\nd', 'b\nc\ne');
    expect(d).toEqual([
      { type: 'del', text: 'a' },
      { type: 'same', text: 'b' },
      { type: 'same', text: 'c' },
      { type: 'del', text: 'd' },
      { type: 'add', text: 'e' },
    ]);
  });
});
