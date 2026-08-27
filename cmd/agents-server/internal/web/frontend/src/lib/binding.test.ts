import { describe, expect, it } from 'vitest';
import { composerSandboxView, groupProjects, projectLabel } from './binding';

const targets = [
  { id: 'tg-a', name: 'host' },
  { id: 'tg-b', name: 'remote' },
];

const projects = [
  { id: 'p1', name: 'api', target_id: 'tg-b', template_id: 'tpl' },
  { id: 'p2', name: 'goagents', target_id: 'tg-a', template_id: 'tpl' },
  { id: 'p3', name: 'scratch', target_id: 'tg-a', template_id: 'tpl' },
  { id: 'p4', name: 'orphan', target_id: 'gone', template_id: 'tpl' },
];

describe('groupProjects', () => {
  it('groups by target in targets order, keeping server order inside', () => {
    const groups = groupProjects(projects, targets);
    expect(groups.map(g => g.targetName)).toEqual(['host', 'remote']);
    expect(groups[0].items.map(p => p.name)).toEqual(['goagents', 'scratch']);
    expect(groups[1].items.map(p => p.name)).toEqual(['api']);
  });
  it('drops projects whose target no longer exists', () => {
    const groups = groupProjects(projects, targets);
    expect(groups.flatMap(g => g.items.map(p => p.id))).not.toContain('p4');
  });
  it('omits empty groups and tolerates missing inputs', () => {
    expect(groupProjects([projects[0]], targets).map(g => g.targetId)).toEqual(['tg-b']);
    expect(groupProjects(null, targets)).toEqual([]);
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
    const v = composerSandboxView(null, projects, targets);
    expect(v.bound).toBe(false);
    expect(v.title).toBe('');
  });

  it('bound: the title carries project and target name', () => {
    const v = composerSandboxView({ projectId: 'p2' }, projects, targets);
    expect(v.bound).toBe(true);
    expect(v.title).toBe('goagents @ host');
  });

  it('bound before the projects arrive: the id, and still bound', () => {
    const v = composerSandboxView({ projectId: 'p-gone' }, projects, targets);
    expect(v.bound).toBe(true);
    expect(v.title).toBe('p-gone');
  });

  it('bound to a project whose target is gone: warning title', () => {
    const v = composerSandboxView({ projectId: 'p4' }, projects, targets);
    expect(v.title).toContain('orphan');
    expect(v.title).toContain('no longer exists');
  });
});
