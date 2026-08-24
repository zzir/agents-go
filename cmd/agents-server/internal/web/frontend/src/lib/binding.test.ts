import { describe, expect, it } from 'vitest';
import { composerSandboxView, groupProjects, projectLabel } from './binding';

const configs = [
  { id: 'sb-a', name: 'host' },
  { id: 'sb-b', name: 'remote' },
];

const projects = [
  { id: 'p1', name: 'api', sandbox_id: 'sb-b' },
  { id: 'p2', name: 'goagents', sandbox_id: 'sb-a' },
  { id: 'p3', name: 'scratch', sandbox_id: 'sb-a' },
  { id: 'p4', name: 'orphan', sandbox_id: 'gone' },
];

describe('groupProjects', () => {
  it('groups by sandbox in configs order, keeping server order inside', () => {
    const groups = groupProjects(projects, configs);
    expect(groups.map(g => g.sandboxName)).toEqual(['host', 'remote']);
    expect(groups[0].items.map(p => p.name)).toEqual(['goagents', 'scratch']);
    expect(groups[1].items.map(p => p.name)).toEqual(['api']);
  });
  it('drops projects whose sandbox config no longer exists', () => {
    const groups = groupProjects(projects, configs);
    expect(groups.flatMap(g => g.items.map(p => p.id))).not.toContain('p4');
  });
  it('omits empty groups and tolerates missing inputs', () => {
    expect(groupProjects([projects[0]], configs).map(g => g.sandboxId)).toEqual(['sb-b']);
    expect(groupProjects(null, configs)).toEqual([]);
    expect(groupProjects(projects, null)).toEqual([]);
  });
});

describe('projectLabel', () => {
  it('joins the project name with the sandbox name', () => {
    expect(projectLabel('goagents', 'host')).toBe('goagents @ host');
  });
});

describe('composerSandboxView', () => {
  it('unbound: no badge', () => {
    const v = composerSandboxView(null, projects, configs);
    expect(v.bound).toBe(false);
    expect(v.title).toBe('');
  });

  it('bound: the title carries sandbox and project name', () => {
    const v = composerSandboxView({ sandboxId: 'sb-a', projectId: 'p2' }, projects, configs);
    expect(v.bound).toBe(true);
    expect(v.title).toBe('host — goagents');
  });

  it('bound to a missing project row: sandbox name alone, never a raw id', () => {
    const v = composerSandboxView({ sandboxId: 'sb-a', projectId: 'p-gone' }, projects, configs);
    expect(v.title).toBe('host');
  });

  it('bound to a deleted sandbox: warning title on the raw id', () => {
    const v = composerSandboxView({ sandboxId: 'gone', projectId: 'p4' }, projects, configs);
    expect(v.title).toContain('gone');
    expect(v.title).toContain('no longer exists');
  });
});
