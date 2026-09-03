import { describe, expect, it } from 'vitest';
import { composerProjectRows, composerSandboxView, projectLabel } from './binding';

const sandboxes = [
  { id: 'sb-a', name: 'host' },
  { id: 'sb-b', name: 'remote' },
];

// Server order is name ASC; the picker re-sorts by creation time.
const projects = [
  { id: 'p1', name: 'api', sandbox_id: 'sb-b', created_at: '2026-09-01T10:00:00Z' },
  { id: 'p2', name: 'goagents', sandbox_id: 'sb-a', created_at: '2026-09-03T10:00:00Z' },
  { id: 'p3', name: 'scratch', sandbox_id: 'sb-a', created_at: '2026-09-02T10:00:00+02:00' },
  { id: 'p4', name: 'orphan', sandbox_id: 'gone', created_at: '2026-09-04T10:00:00Z' },
];

describe('composerProjectRows', () => {
  it('lists newest first, each row with its sandbox name', () => {
    const rows = composerProjectRows(projects, sandboxes);
    expect(rows.map(r => r.project.name)).toEqual(['goagents', 'scratch', 'api']);
    expect(rows.map(r => r.sandboxName)).toEqual(['host', 'host', 'remote']);
  });
  it('drops projects whose sandbox no longer exists', () => {
    expect(composerProjectRows(projects, sandboxes).map(r => r.project.id)).not.toContain('p4');
  });
  it('breaks a missing or equal timestamp on the id, later id first', () => {
    const rows = composerProjectRows([
      { id: 'a', name: 'x', sandbox_id: 'sb-a' },
      { id: 'b', name: 'y', sandbox_id: 'sb-a' },
    ], sandboxes);
    expect(rows.map(r => r.project.id)).toEqual(['b', 'a']);
  });
  it('tolerates missing inputs', () => {
    expect(composerProjectRows(null, sandboxes)).toEqual([]);
    expect(composerProjectRows(projects, null)).toEqual([]);
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
