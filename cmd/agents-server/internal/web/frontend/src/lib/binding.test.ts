import { describe, expect, it } from 'vitest';
import { bindingWorkDirIssue, composerSandboxView, groupProjects, projectBase, projectLabel, recentProjects, workDirBasename, workspaceSubdirValid } from './binding';

describe('workspaceSubdirValid', () => {
  it('accepts empty, the mount point, and clean subtrees', () => {
    expect(workspaceSubdirValid('')).toBe(true);
    expect(workspaceSubdirValid('/workspace')).toBe(true);
    expect(workspaceSubdirValid('/workspace/proj')).toBe(true);
    expect(workspaceSubdirValid('/workspace/a/b/../c')).toBe(true); // stays inside
  });
  it('rejects anything outside the mount', () => {
    expect(workspaceSubdirValid('/etc')).toBe(false);
    expect(workspaceSubdirValid('relative')).toBe(false);
    expect(workspaceSubdirValid('/workspacex/f')).toBe(false);
    expect(workspaceSubdirValid('/workspace/../etc')).toBe(false);
  });
});

describe('recentProjects', () => {
  const configs = [
    { id: 'sb-a', name: 'host' },
    { id: 'sb-b', name: 'remote' },
  ];
  it('aggregates distinct (sandbox, workdir) pairs, newest first, skipping unbound and deleted', () => {
    const sessions = [
      { sandbox_id: 'sb-a', work_dir: '/w/one' },
      { sandbox_id: '', work_dir: '' },                 // unbound — skipped
      { sandbox_id: 'sb-a', work_dir: '/w/one/' },      // trailing slash — same project
      { sandbox_id: 'sb-b', work_dir: '' },             // default dir is a valid project
      { sandbox_id: 'gone', work_dir: '/x' },           // deleted sandbox — dropped
    ];
    const got = recentProjects(sessions, configs);
    expect(got.map(p => p.base)).toEqual(['one', 'default']);
    expect(got.map(p => p.title)).toEqual(['/w/one @ host', '(sandbox default) @ remote']);
  });
  it('drops docker pairs outside /workspace (the runtime treats them as the default)', () => {
    const dockerConfigs = [{ id: 'sb-d', name: 'devbox', type: 'docker' }];
    const sessions = [
      { sandbox_id: 'sb-d', work_dir: '/tmp/test' },       // stale host path — dropped
      { sandbox_id: 'sb-d', work_dir: '/workspace/app' },  // real project
    ];
    expect(recentProjects(sessions, dockerConfigs).map(p => p.base)).toEqual(['app']);
  });
  it('caps the list and tolerates missing inputs', () => {
    const many = Array.from({ length: 20 }, (_, i) => ({ sandbox_id: 'sb-a', work_dir: `/w/${i}` }));
    expect(recentProjects(many, configs, 3)).toHaveLength(3);
    expect(recentProjects(null, configs)).toEqual([]);
    expect(recentProjects([], null)).toEqual([]);
  });
  it('groups by sandbox preserving recency order', () => {
    const sessions = [
      { sandbox_id: 'sb-a', work_dir: '/w/one' },
      { sandbox_id: 'sb-b', work_dir: '/srv/x' },
      { sandbox_id: 'sb-a', work_dir: '/w/two' },
    ];
    const groups = groupProjects(recentProjects(sessions, configs));
    expect(groups.map(g => g.sandboxName)).toEqual(['host', 'remote']);
    expect(groups[0].items.map(p => p.base)).toEqual(['one', 'two']);
  });
});

describe('projectLabel', () => {
  it('joins the directory basename with the sandbox name', () => {
    expect(projectLabel('/srv/apps/goagents', 'host')).toBe('goagents @ host');
    expect(projectLabel('', 'remote')).toBe('default @ remote');
    expect(projectLabel('/workspace', 'docker')).toBe('workspace @ docker');
  });
});

describe('workDirBasename', () => {
  it('takes the last segment, ignoring trailing slashes', () => {
    expect(workDirBasename('/srv/app/proj')).toBe('proj');
    expect(workDirBasename('/srv/app/proj/')).toBe('proj');
    expect(workDirBasename('proj')).toBe('proj');
  });
  it('keeps the root and empty distinguishable', () => {
    expect(workDirBasename('/')).toBe('/');
    expect(workDirBasename('')).toBe('');
  });
});

const configs = [
  { id: 'sb-ssh', name: 'build box', default_work_dir: '/srv/app', work_dir_editable: true },
  { id: 'sb-docker', name: 'container', default_work_dir: '/ws', work_dir_editable: false },
];

describe('composerSandboxView', () => {
  it('unbound with nothing selected: no badge, nothing to send', () => {
    const v = composerSandboxView(null, undefined, configs, '');
    expect(v.bound).toBe(false);
    expect(v.effectiveWorkDir).toBe('');
  });

  it('unbound editable sandbox: draft overrides the default', () => {
    const base = composerSandboxView(null, configs[0], configs, '');
    expect(base.effectiveWorkDir).toBe('/srv/app');
    const drafted = composerSandboxView(null, configs[0], configs, '/home/me/proj');
    expect(drafted.effectiveWorkDir).toBe('/home/me/proj');
  });

  it('unbound non-editable sandbox sends no directory claim', () => {
    // Its directory is fixed; snapshotting the advertised default would send
    // a claim the server refuses (the ephemeral-docker regression).
    const v = composerSandboxView(null, configs[1], configs, '/ignored');
    expect(v.effectiveWorkDir).toBe('');
  });

  it('bound: the title carries name and workdir', () => {
    const v = composerSandboxView({ sandboxId: 'sb-ssh', workDir: '/srv/app' }, undefined, configs, '');
    expect(v.bound).toBe(true);
    expect(v.title).toContain('build box');
    expect(v.title).toContain('/srv/app');
    expect(v.effectiveWorkDir).toBe('/srv/app');
  });

  it('bound with empty workdir: the title says default directory', () => {
    const v = composerSandboxView({ sandboxId: 'sb-ssh', workDir: '' }, undefined, configs, '');
    expect(v.title).toContain('default directory');
  });

  it('bound to a deleted sandbox: warning title on the raw id', () => {
    const v = composerSandboxView({ sandboxId: 'gone', workDir: '/w' }, undefined, configs, '');
    expect(v.title).toContain('gone');
    expect(v.title).toContain('no longer exists');
  });

  it('bound wins even with a selection and draft present', () => {
    const v = composerSandboxView({ sandboxId: 'sb-docker', workDir: '/ws' }, configs[0], configs, '/draft');
    expect(v.bound).toBe(true);
    expect(v.effectiveWorkDir).toBe('/ws');
  });
});

describe('projectBase', () => {
  it('names a workdir by its basename, empty by the default', () => {
    expect(projectBase('/srv/apps/goagents')).toBe('goagents');
    expect(projectBase('')).toBe('default');
    expect(projectBase('/')).toBe('/');
  });
});

describe('bindingWorkDirIssue', () => {
  const ssh = { id: 's', name: 's', type: 'ssh', work_dir_editable: true };
  const sshHome = { ...ssh, default_work_dir: '/srv' };
  const sshRelHome = { ...ssh, default_work_dir: 'projects' };
  const dockerP = { id: 'd', name: 'd', type: 'docker', default_work_dir: '/workspace', work_dir_editable: true };
  const dockerE = { ...dockerP, work_dir_editable: false };
  const local = { id: 'l', name: 'l', type: 'local', default_work_dir: '/ws', work_dir_editable: true };

  it('passes what the server would bind', () => {
    expect(bindingWorkDirIssue(sshHome, '')).toBeNull();
    expect(bindingWorkDirIssue(ssh, '/srv/app')).toBeNull();
    expect(bindingWorkDirIssue(dockerP, '/workspace/proj')).toBeNull();
    expect(bindingWorkDirIssue(dockerP, '')).toBeNull();
    expect(bindingWorkDirIssue(local, '')).toBeNull();
    expect(bindingWorkDirIssue(local, '/abs')).toBeNull();
    expect(bindingWorkDirIssue(undefined, 'anything')).toBeNull();
  });

  it('a fixed-directory backend has nothing to validate', () => {
    expect(bindingWorkDirIssue(dockerE, '/tmp/x')).toBeNull();
  });

  it('refuses what the server would refuse', () => {
    expect(bindingWorkDirIssue(ssh, '')).toMatch(/no default directory/);
    expect(bindingWorkDirIssue(ssh, 'projects/app')).toMatch(/absolute/);
    // The dialog gap: an empty draft over a RELATIVE config default must be
    // caught here, not first at send time.
    expect(bindingWorkDirIssue(sshRelHome, '')).toMatch(/absolute/);
    expect(bindingWorkDirIssue(dockerP, '/tmp/project')).toMatch(/workspace/);
    expect(bindingWorkDirIssue(local, 'rel/path')).toMatch(/absolute/);
  });
});
