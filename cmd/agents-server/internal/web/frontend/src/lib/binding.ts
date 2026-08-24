/* Pure projection of the composer's sandbox area — kept out of the component
   so the node-environment vitest suite can cover it. */

export interface SandboxConfigLite {
  id: string;
  name: string;
  type?: string;
  default_work_dir?: string;
  work_dir_editable?: boolean;
}

/* Whether a docker project directory stays inside the /workspace mount:
   /workspace itself, or a subdirectory that no ".." sequence escapes. Empty
   means "the default" and is always fine. */
export function workspaceSubdirValid(p: string): boolean {
  if (!p || p === '/workspace') return true;
  if (!p.startsWith('/workspace/')) return false;
  let depth = 0;
  for (const seg of p.slice('/workspace/'.length).split('/')) {
    if (seg === '' || seg === '.') continue;
    if (seg === '..') {
      depth--;
      if (depth < 0) return false;
      continue;
    }
    depth++;
  }
  return true;
}

/* The session's permanent (sandbox_id, work_dir) binding, or null while unbound. */
export interface SessionBinding {
  sandboxId: string;
  workDir: string;
}

export interface ComposerSandboxView {
  /* Bound: the picker is replaced by a read-only badge. */
  bound: boolean;
  /* Tooltip: the full story, incl. the empty-workDir explanation. */
  title: string;
  /* Bound: the binding's workdir. Unbound: what a bind would store right now
     — the draft over the sandbox default for a backend that honors a custom
     directory, '' for one that does not (its directory is fixed; snapshotting
     it would send a directory claim the server refuses). */
  effectiveWorkDir: string;
}

/* The last path segment — what a person recognizes a directory by (the way
   editors and terminal prompts show a project). "/" stays "/", trailing
   slashes are ignored, "" stays "" (caller renders its own placeholder). */
export function workDirBasename(path: string): string {
  const trimmed = path.replace(/\/+$/, '');
  if (!trimmed) return path ? '/' : '';
  const i = trimmed.lastIndexOf('/');
  return i >= 0 ? trimmed.slice(i + 1) : trimmed;
}

/* The name a project row or badge shows for a workdir: its basename, or
   'default' for the sandbox's own directory. */
export function projectBase(workDir: string): string {
  return workDir ? workDirBasename(workDir) : 'default';
}

/* One entry of the composer's project picker: a (sandbox, workdir) pair a
   person recognizes as "a project" — the unit every other agent product makes
   first-class. Derived, not stored: the recent list is an aggregation over
   existing session bindings. */
export interface ProjectOption {
  sandboxId: string;
  workDir: string; // '' = the sandbox's default directory
  sandboxName: string;
  /* The bare project name — what a grouped menu row shows. */
  base: string;
  /* Full path for the hover title. */
  title: string;
}

export function projectLabel(workDir: string, sandboxName: string): string {
  return `${projectBase(workDir)} @ ${sandboxName}`;
}

/* Distinct (sandbox_id, work_dir) pairs from bound sessions, newest first
   (the sessions list arrives sorted by updated_at DESC). Pairs whose sandbox
   config no longer exists are dropped — they cannot be started again. */
export function recentProjects(
  sessions: Array<{ sandbox_id?: string; work_dir?: string }> | null,
  configs: SandboxConfigLite[] | null,
  limit = 8,
): ProjectOption[] {
  if (!sessions || !configs || configs.length === 0) return [];
  const seen = new Set<string>();
  const out: ProjectOption[] = [];
  for (const s of sessions) {
    if (!s.sandbox_id) continue;
    // Normalize trailing slashes so "/a/b" and "/a/b/" are one project,
    // not two menu rows.
    let workDir = (s.work_dir || '').trim();
    if (workDir !== '/') workDir = workDir.replace(/\/+$/, '');
    const key = s.sandbox_id + '\0' + workDir;
    if (seen.has(key)) continue;
    seen.add(key);
    const cfg = configs.find(c => c.id === s.sandbox_id);
    if (!cfg) continue;
    // A docker pair outside /workspace is a binding the runtime treats as the
    // default (see the server's out-of-tree fallback); re-offering it would
    // copy the stale value into fresh sessions.
    if (cfg.type === 'docker' && !workspaceSubdirValid(workDir)) continue;
    out.push({
      sandboxId: s.sandbox_id,
      workDir,
      sandboxName: cfg.name,
      base: projectBase(workDir),
      title: `${workDir || '(sandbox default)'} @ ${cfg.name}`,
    });
    if (out.length >= limit) break;
  }
  return out;
}

/* The picker's grouped view: one group per sandbox, insertion (= recency)
   order for both the groups and the projects inside them. */
export function groupProjects(projects: ProjectOption[]): Array<{ sandboxId: string; sandboxName: string; items: ProjectOption[] }> {
  const groups: Array<{ sandboxId: string; sandboxName: string; items: ProjectOption[] }> = [];
  for (const p of projects) {
    let g = groups.find(x => x.sandboxId === p.sandboxId);
    if (!g) {
      g = { sandboxId: p.sandboxId, sandboxName: p.sandboxName, items: [] };
      groups.push(g);
    }
    g.items.push(p);
  }
  return groups;
}

/* Why the given workdir draft could not bind on this sandbox, or null when it
   can — the client-side mirror of the server's ResolveBindingWorkDir, in ONE
   place so the dialog's validation and the send-time guard cannot drift. A
   sandbox with a fixed directory (work_dir_editable false) sends no directory
   claim, so any draft is moot; otherwise the draft must be /workspace or a
   subtree of it. */
export function bindingWorkDirIssue(cfg: SandboxConfigLite | undefined, draft: string): string | null {
  if (!cfg || !cfg.work_dir_editable) return null;
  return workspaceSubdirValid(draft.trim()) ? null : 'Must be /workspace or a subdirectory of it.';
}

export function composerSandboxView(
  binding: SessionBinding | null,
  selected: SandboxConfigLite | undefined,
  configs: SandboxConfigLite[] | null,
  draftWorkDir: string,
): ComposerSandboxView {
  if (binding && binding.sandboxId) {
    const cfg = configs?.find(c => c.id === binding.sandboxId);
    const name = cfg?.name || binding.sandboxId;
    let title = binding.workDir
      ? `${name} — ${binding.workDir}`
      : `${name} — sandbox default directory`;
    if (!cfg) title = `${binding.sandboxId} — this sandbox no longer exists; runs fail until it is restored`;
    return { bound: true, title, effectiveWorkDir: binding.workDir };
  }
  const effective = selected?.work_dir_editable
    ? (draftWorkDir || selected?.default_work_dir || '')
    : '';
  return { bound: false, title: '', effectiveWorkDir: effective };
}
