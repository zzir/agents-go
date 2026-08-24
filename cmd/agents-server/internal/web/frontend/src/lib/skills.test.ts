import { describe, expect, it } from 'vitest';
import { groupBySource, splitLocalByOwner, type Skill } from './skills';

const sk = (name: string, over: Partial<Skill> = {}): Skill =>
  ({ id: name, name, description: '', ...over });

describe('splitLocalByOwner', () => {
  const me = 'admin-1';
  const label = (id: string) => ({ 'u-2': 'two@example.com' } as Record<string, string>)[id] || id;

  it('keeps own and global rows under Local, splits foreign owners out', () => {
    const groups = groupBySource([
      sk('mine', { owner_id: me, scope: 'private' }),
      sk('shared', { scope: 'global' }),
      sk('theirs', { owner_id: 'u-2', scope: 'private' }),
      sk('imported', { owner_id: 'u-2', scope: 'private', source_repo: 'https://github.com/o/r' }),
    ]);
    const split = splitLocalByOwner(groups, me, label);
    expect(split.map(g => g.label)).toEqual(['Local', 'Local — two@example.com', 'github.com/o/r']);
    expect(split[0].skills.map(s => s.name).sort()).toEqual(['mine', 'shared']);
    expect(split[1].skills.map(s => s.name)).toEqual(['theirs']);
    // Split groups render under distinct keys despite sharing repo ''.
    expect(split[1].key).toBe('local-u-2');
  });

  it('is a no-op without a Local bucket', () => {
    const groups = groupBySource([sk('imported', { source_repo: 'https://github.com/o/r' })]);
    expect(splitLocalByOwner(groups, me, label)).toEqual(groups);
  });

  it('drops an empty Local group when every row is foreign', () => {
    const groups = groupBySource([sk('theirs', { owner_id: 'u-2', scope: 'private' })]);
    const split = splitLocalByOwner(groups, me, label);
    expect(split.map(g => g.label)).toEqual(['Local — two@example.com']);
  });
});
