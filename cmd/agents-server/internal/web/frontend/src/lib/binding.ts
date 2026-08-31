/* Pure projection of the composer's project area — kept out of the component
   so the node-environment vitest suite can cover it. */

/* What a sandbox backend can do, as its API row declares it. The UI offers a
   capability only when it is declared true — an undefined `supports` (older
   server, row still loading) offers nothing beyond the safe baseline. */
export interface SandboxSupports {
  /* The container can be rebuilt in place. */
  rebuild?: boolean;
}

export interface SandboxLite {
  id: string;
  name: string;
  /* docker | e2b — display and form data only; capabilities come from
     `supports`, never from sniffing this. */
  type?: string;
  supports?: SandboxSupports;
}

/* A project row as GET /projects returns it: the named working tree a
   sandbox's container mounts. */
export interface Project {
  id: string;
  name: string;
  sandbox_id: string;
  /* The volume the files live in, and the daemon it is on — shown by the
     delete dialog, which destroys it. */
  storage_hint?: string;
  /* The row's write counter: an update sends back the one it was edited
     against, and a concurrent write answers 409. */
  revision?: number;
  /* How many sessions bind this project — the confirm step says how many an
     environment change reaches. */
  session_count?: number;
}

/* One environment variable of a project. Values are write-only: the server
   masks every one on the way out, and nothing here hides a value from the
   agent, which reads the container's environment with one command. */
export interface EnvVar {
  key: string;
  value: string;
}

/* GET /projects/:id — the one response that carries an environment, as names
   with masked values. */
export interface ProjectDetail extends Project {
  env?: EnvVar[];
}

/* The sentinel every stored value comes back as; sending it back unchanged
   keeps what is stored. */
export const SECRET_MASK = '********';

/* The session's permanent project binding, or null while unbound. The project
   pins the sandbox, so one id is the whole binding. */
export interface SessionBinding {
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

/* The picker's grouped view: one group per sandbox, groups in sandbox order,
   items in server order (name ASC). Projects whose sandbox no longer exists
   are dropped — they cannot be started again. */
export function groupProjects(
  projects: Project[] | null,
  sandboxes: SandboxLite[] | null,
): Array<{ sandboxId: string; sandboxName: string; items: Project[] }> {
  if (!projects || !sandboxes) return [];
  const groups: Array<{ sandboxId: string; sandboxName: string; items: Project[] }> = [];
  for (const sb of sandboxes) {
    const items = projects.filter(p => p.sandbox_id === sb.id);
    if (items.length > 0) groups.push({ sandboxId: sb.id, sandboxName: sb.name, items });
  }
  return groups;
}

export function composerSandboxView(
  binding: SessionBinding | null,
  projects: Project[] | null,
  sandboxes: SandboxLite[] | null,
): ComposerSandboxView {
  if (binding && binding.projectId) {
    const project = projects?.find(p => p.id === binding.projectId);
    if (!project) {
      // Either the projects fetch is still in flight or the row is gone; the
      // id is all there is to say.
      return { bound: true, title: binding.projectId };
    }
    const sb = sandboxes?.find(s => s.id === project.sandbox_id);
    const title = sb
      ? projectLabel(project.name, sb.name)
      : `${project.name} — this project's machine no longer exists; runs fail until it is restored`;
    return { bound: true, title };
  }
  return { bound: false, title: '' };
}
