import { describe, expect, it } from 'vitest';
import { composerSandboxView, groupProjects, projectLabel } from './binding';

const sandboxes = [
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
  it('groups by sandbox in sandbox order, keeping server order inside', () => {
    const groups = groupProjects(projects, sandboxes);
    expect(groups.map(g => g.sandboxName)).toEqual(['host', 'remote']);
    expect(groups[0].items.map(p => p.name)).toEqual(['goagents', 'scratch']);
    expect(groups[1].items.map(p => p.name)).toEqual(['api']);
  });
  it('drops projects whose sandbox no longer exists', () => {
    const groups = groupProjects(projects, sandboxes);
    expect(groups.flatMap(g => g.items.map(p => p.id))).not.toContain('p4');
  });
  it('omits empty groups and tolerates missing inputs', () => {
    expect(groupProjects([projects[0]], sandboxes).map(g => g.sandboxId)).toEqual(['sb-b']);
    expect(groupProjects(null, sandboxes)).toEqual([]);
    expect(groupProjects(projects, null)).toEqual([]);
  });
});

describe('projectLabel', () => {
  it('joins the project name with the target name', () => {
    expect(projectLabel('goagents', 'host')).toBe('goagents @ host');
  });
});

describe('composerSandboxView', () => {
  it('unbound: no badge', () => {
    const v = composerSandboxView(null, projects, sandboxes);
    expect(v.bound).toBe(false);
    expect(v.title).toBe('');
  });

  it('bound: the title carries project and target name', () => {
    const v = composerSandboxView({ projectId: 'p2' }, projects, sandboxes);
    expect(v.bound).toBe(true);
    expect(v.title).toBe('goagents @ host');
  });

  it('bound before the projects arrive: the id, and still bound', () => {
    const v = composerSandboxView({ projectId: 'p-gone' }, projects, sandboxes);
    expect(v.bound).toBe(true);
    expect(v.title).toBe('p-gone');
  });

  it('bound to a project whose target is gone: warning title', () => {
    const v = composerSandboxView({ projectId: 'p4' }, projects, sandboxes);
    expect(v.title).toContain('orphan');
    expect(v.title).toContain('no longer exists');
  });
});
