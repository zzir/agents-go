/* Pure projection of the composer's sandbox area — kept out of the component
   so the node-environment vitest suite can cover it. */

export interface SandboxConfigLite {
  id: string;
  name: string;
}

/* A project row as GET /projects returns it: the named working tree a
   sandbox's container mounts. */
export interface Project {
  id: string;
  name: string;
  sandbox_id: string;
}

/* The session's permanent (sandbox_id, project_id) binding, or null while unbound. */
export interface SessionBinding {
  sandboxId: string;
  projectId: string;
}

export interface ComposerSandboxView {
  /* Bound: the picker is replaced by a read-only badge. */
  bound: boolean;
  /* Tooltip: the full story. */
  title: string;
}

export function projectLabel(projectName: string, sandboxName: string): string {
  return `${projectName} @ ${sandboxName}`;
}

/* The picker's grouped view: one group per sandbox, groups in configs order,
   items in server order (name ASC). Projects whose sandbox config no longer
   exists are dropped — they cannot be started again. */
export function groupProjects(
  projects: Project[] | null,
  configs: SandboxConfigLite[] | null,
): Array<{ sandboxId: string; sandboxName: string; items: Project[] }> {
  if (!projects || !configs) return [];
  const groups: Array<{ sandboxId: string; sandboxName: string; items: Project[] }> = [];
  for (const cfg of configs) {
    const items = projects.filter(p => p.sandbox_id === cfg.id);
    if (items.length > 0) groups.push({ sandboxId: cfg.id, sandboxName: cfg.name, items });
  }
  return groups;
}

export function composerSandboxView(
  binding: SessionBinding | null,
  projects: Project[] | null,
  configs: SandboxConfigLite[] | null,
): ComposerSandboxView {
  if (binding && binding.sandboxId) {
    const cfg = configs?.find(c => c.id === binding.sandboxId);
    const name = cfg?.name || binding.sandboxId;
    const projectName = projects?.find(p => p.id === binding.projectId)?.name || binding.projectId;
    let title = projectName ? `${name} — ${projectName}` : name;
    if (!cfg) title = `${binding.sandboxId} — this sandbox no longer exists; runs fail until it is restored`;
    return { bound: true, title };
  }
  return { bound: false, title: '' };
}
