/* Pure projection of the composer's workspace area — kept out of the component
   so the node-environment vitest suite can cover it. */

/* What a sandbox backend can do, as its API row declares it: a capability is
   offered only when declared true; an undefined `supports` offers the safe baseline. */
export interface SandboxSupports {
  /* The container can be rebuilt in place. */
  rebuild?: boolean;
  /* Every port inside the sandbox is public at <port>-<sandbox id>.<domain>. */
  public_host?: boolean;
}

export interface SandboxLite {
  id: string;
  name: string;
  /* docker | e2b — display and form data only; capabilities come from
     `supports`, never from sniffing this. */
  type?: string;
  supports?: SandboxSupports;
}

/* A workspace row as GET /workspaces returns it: the named working tree a
   sandbox's container mounts. */
export interface Workspace {
  id: string;
  name: string;
  sandbox_id: string;
  /* When the row was created (RFC 3339) — the composer picker lists newest
     first. */
  created_at?: string;
  /* The volume the files live in, and the daemon it is on — shown by the
     delete dialog, which destroys it. */
  storage_hint?: string;
  /* The row's write counter: an update sends back the one it was edited
     against, and a concurrent write answers 409. */
  revision?: number;
  /* How many sessions bind this workspace — the confirm step says how many an
     environment change reaches. */
  session_count?: number;
}

/* One environment variable of a workspace. Values are write-only: the server
   masks every one on the way out (the agent reads the container's environment anyway). */
export interface EnvVar {
  key: string;
  value: string;
}

/* GET /workspaces/:id — the one response that carries an environment, as names
   with masked values. */
export interface WorkspaceDetail extends Workspace {
  env?: EnvVar[];
}

/* The sentinel every stored value comes back as; sending it back unchanged
   keeps what is stored. */
export const SECRET_MASK = '********';

/* The session's permanent workspace binding, or null while unbound. The workspace
   pins the sandbox, so one id is the whole binding. */
export interface SessionBinding {
  workspaceId: string;
}

export interface ComposerSandboxView {
  /* Bound: the picker is replaced by a read-only badge. */
  bound: boolean;
  /* Tooltip: the full story. */
  title: string;
}

export function workspaceLabel(workspaceName: string, sandboxName: string): string {
  return `${workspaceName} @ ${sandboxName}`;
}

/* The composer picker's rows: newest workspace first, with its sandbox's name; a
   workspace whose sandbox no longer exists is dropped (it cannot start). */
export function composerWorkspaceRows(
  workspaces: Workspace[] | null,
  sandboxes: SandboxLite[] | null,
): Array<{ workspace: Workspace; sandboxName: string }> {
  if (!workspaces || !sandboxes) return [];
  const names = new Map(sandboxes.map(sb => [sb.id, sb.name]));
  const created = (p: Workspace) => (p.created_at ? Date.parse(p.created_at) || 0 : 0);
  return workspaces
    .filter(p => names.has(p.sandbox_id))
    .sort((a, b) => created(b) - created(a) || b.id.localeCompare(a.id))
    .map(p => ({ workspace: p, sandboxName: names.get(p.sandbox_id)! }));
}

export function composerSandboxView(
  binding: SessionBinding | null,
  workspaces: Workspace[] | null,
  sandboxes: SandboxLite[] | null,
): ComposerSandboxView {
  if (binding && binding.workspaceId) {
    const workspace = workspaces?.find(p => p.id === binding.workspaceId);
    if (!workspace) {
      // Either the workspaces fetch is still in flight or the row is gone; the
      // id is all there is to say.
      return { bound: true, title: binding.workspaceId };
    }
    const sb = sandboxes?.find(s => s.id === workspace.sandbox_id);
    const title = sb
      ? workspaceLabel(workspace.name, sb.name)
      : `${workspace.name} — this workspace's machine no longer exists; runs fail until it is restored`;
    return { bound: true, title };
  }
  return { bound: false, title: '' };
}
