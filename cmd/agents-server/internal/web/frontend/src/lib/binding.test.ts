import { describe, expect, it } from 'vitest';
import { composerWorkspaceRows, composerSandboxView, workspaceLabel } from './binding';

const sandboxes = [
  { id: 'sb-a', name: 'host' },
  { id: 'sb-b', name: 'remote' },
];

// Server order is name ASC; the picker re-sorts by creation time.
const workspaces = [
  { id: 'p1', name: 'api', sandbox_id: 'sb-b', created_at: '2026-09-01T10:00:00Z' },
  { id: 'p2', name: 'goagents', sandbox_id: 'sb-a', created_at: '2026-09-03T10:00:00Z' },
  { id: 'p3', name: 'scratch', sandbox_id: 'sb-a', created_at: '2026-09-02T10:00:00+02:00' },
  { id: 'p4', name: 'orphan', sandbox_id: 'gone', created_at: '2026-09-04T10:00:00Z' },
];

describe('composerWorkspaceRows', () => {
  it('lists newest first, each row with its sandbox name', () => {
    const rows = composerWorkspaceRows(workspaces, sandboxes);
    expect(rows.map(r => r.workspace.name)).toEqual(['goagents', 'scratch', 'api']);
    expect(rows.map(r => r.sandboxName)).toEqual(['host', 'host', 'remote']);
  });
  it('drops workspaces whose sandbox no longer exists', () => {
    expect(composerWorkspaceRows(workspaces, sandboxes).map(r => r.workspace.id)).not.toContain('p4');
  });
  it('breaks a missing or equal timestamp on the id, later id first', () => {
    const rows = composerWorkspaceRows([
      { id: 'a', name: 'x', sandbox_id: 'sb-a' },
      { id: 'b', name: 'y', sandbox_id: 'sb-a' },
    ], sandboxes);
    expect(rows.map(r => r.workspace.id)).toEqual(['b', 'a']);
  });
  it('tolerates missing inputs', () => {
    expect(composerWorkspaceRows(null, sandboxes)).toEqual([]);
    expect(composerWorkspaceRows(workspaces, null)).toEqual([]);
  });
});

describe('workspaceLabel', () => {
  it('joins the workspace name with the target name', () => {
    expect(workspaceLabel('goagents', 'host')).toBe('goagents @ host');
  });
});

describe('composerSandboxView', () => {
  it('unbound: no badge', () => {
    const v = composerSandboxView(null, workspaces, sandboxes);
    expect(v.bound).toBe(false);
    expect(v.title).toBe('');
  });

  it('bound: the title carries workspace and target name', () => {
    const v = composerSandboxView({ workspaceId: 'p2' }, workspaces, sandboxes);
    expect(v.bound).toBe(true);
    expect(v.title).toBe('goagents @ host');
  });

  it('bound before the workspaces arrive: the id, and still bound', () => {
    const v = composerSandboxView({ workspaceId: 'p-gone' }, workspaces, sandboxes);
    expect(v.bound).toBe(true);
    expect(v.title).toBe('p-gone');
  });

  it('bound to a workspace whose target is gone: warning title', () => {
    const v = composerSandboxView({ workspaceId: 'p4' }, workspaces, sandboxes);
    expect(v.title).toContain('orphan');
    expect(v.title).toContain('no longer exists');
  });
});
