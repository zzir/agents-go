import { describe, expect, it } from 'vitest';
import { groupSkills, qualifiedName, repoLabel, type Skill } from './skills';

const sk = (name: string, over: Partial<Skill> = {}): Skill =>
  ({ id: name, name, description: '', ...over });

const me = 'admin-1';

describe('repoLabel / qualifiedName', () => {
  it('qualifies a github source by owner/repo and any other by host', () => {
    expect(repoLabel('https://github.com/o/r')).toBe('o/r');
    expect(repoLabel('https://example.com/some/path/SKILL.md')).toBe('example.com');
    expect(repoLabel('')).toBe('');
  });

  it('leaves a workbench-authored skill unqualified', () => {
    expect(qualifiedName(sk('local'))).toBe('local');
    expect(qualifiedName(sk('docx', { source_repo: 'https://github.com/o/r' }))).toBe('o/r:docx');
  });
});

describe('groupSkills', () => {
  // The rows arrive in the server's order (published first, newest first), so
  // the groups keep it — only the published ones are lifted, as the flat
  // listings do.
  it('groups an imported repo per owner, published groups first', () => {
    const groups = groupSkills([
      sk('a', { owner_id: me, scope: 'global', source_repo: 'https://github.com/o/r' }),
      sk('b', { owner_id: me, scope: 'global', source_repo: 'https://github.com/o/r' }),
      sk('mine', { owner_id: me, scope: 'private' }),
      sk('theirs', { owner_id: 'u-2', scope: 'private', source_repo: 'https://github.com/o/r' }),
    ]);
    expect(groups.map(g => g.label)).toEqual(['o/r', 'Local', 'o/r']);
    expect(groups[0].skills.map(s => s.name).sort()).toEqual(['a', 'b']);
    expect(groups[0].scope).toBe('global');
    expect(groups[2].ownerId).toBe('u-2');
    // Same repo, two owners: distinct render keys.
    expect(groups[0].key).not.toBe(groups[2].key);
  });

  it('keeps the order the rows arrived in within a scope', () => {
    const groups = groupSkills([
      sk('newest', { owner_id: me, scope: 'private', source_repo: 'https://github.com/o/new' }),
      sk('older', { owner_id: me, scope: 'private', source_repo: 'https://github.com/o/old' }),
    ]);
    expect(groups.map(g => g.label)).toEqual(['o/new', 'o/old']);
  });

  it('leaves a mixed Local bucket without a group scope', () => {
    const groups = groupSkills([
      sk('mine', { owner_id: me, scope: 'private' }),
      sk('published', { owner_id: me, scope: 'global' }),
    ]);
    expect(groups).toHaveLength(1);
    expect(groups[0].scope).toBeUndefined();
  });

  it('buckets another member into their own group without naming them', () => {
    const groups = groupSkills([
      sk('mine', { owner_id: me, scope: 'private' }),
      sk('theirs', { owner_id: 'u-2', scope: 'private' }),
    ]);
    expect(groups.map(g => g.label)).toEqual(['Local', 'Local']);
    expect(groups[0].key).not.toBe(groups[1].key);
  });
});
