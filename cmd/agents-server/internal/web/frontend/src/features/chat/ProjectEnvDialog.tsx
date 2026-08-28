import { useEffect, useState } from 'react';
import { Dialog, Flash, FormControl, Spinner, TextInput } from '@primer/react';
import type { ReactElement } from 'react';
import { api } from '@/lib/api';
import { toast } from '@/lib/toast';
import { EnvEditor, cleanEnv, envError } from '@/components/EnvEditor';
import type { EnvVar, Project, ProjectDetail } from '@/lib/binding';

/* The project's settings: the environment (loaded on open — a listing never
   carries one) and the ports its container publishes. Values arrive masked and
   go back masked unless the person rewrites one.

   Saving a CHANGED environment or port list replaces the container at the
   project's next run, so the confirm step spells out what that costs; a save
   that rewrote nothing goes through without asking. */

interface ProjectEnvDialogProps {
  project: Project;
  sessionCount?: number;
  onClose: () => void;
}

/* The sandbox's settings, read-only: the image the container starts from and
   whether it has a network. Both answer questions this dialog provokes —
   "why can't the setup reach the internet?" above all — and both are the
   admin's to change, in Settings. `type` decides whether ports are shown at
   all: on an E2B-compatible service every port is already published. */
interface SandboxSummary {
  image?: string;
  network?: string;
}

/* What the container is created with — the comparison the server makes to
   decide whether to replace it. Untouched rows compare equal because both
   sides still hold the mask. */
const containerEnv = (vars: EnvVar[]) =>
  JSON.stringify(cleanEnv(vars).map(v => [v.key, v.value]).sort((a, b) => a[0].localeCompare(b[0])));

/* "3000, 5173" -> [3000, 5173]; null when any entry is not a port. Empty is
   an empty list, not an error. */
export function parsePorts(raw: string): number[] | null {
  const parts = raw.split(',').map(s => s.trim()).filter(Boolean);
  const out: number[] = [];
  for (const p of parts) {
    if (!/^\d+$/.test(p)) return null;
    const n = Number(p);
    if (n < 1 || n > 65535) return null;
    out.push(n);
  }
  return out;
}

export function ProjectEnvDialog({ project, sessionCount, onClose }: ProjectEnvDialogProps): ReactElement {
  const [vars, setVars] = useState<EnvVar[] | null>(null);
  const [ports, setPorts] = useState('');
  const [loadedPorts, setLoadedPorts] = useState('');
  const [sandbox, setSandbox] = useState<SandboxSummary | null>(null);
  const [sandboxType, setSandboxType] = useState('');
  const [revision, setRevision] = useState<number | undefined>(project.revision);
  const [loaded, setLoaded] = useState('');
  const [saving, setSaving] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let live = true;
    api.projects.get(project.id)
      .then(d => {
        if (!live) return;
        const env = ((d as ProjectDetail).env || []) as EnvVar[];
        setVars(env);
        setRevision((d as ProjectDetail).revision);
        setLoaded(containerEnv(env));
        const list = ((d as ProjectDetail).ports || []).join(', ');
        setPorts(list);
        setLoadedPorts(list);
      })
      .catch((e: Error) => { if (live) setError(e.message || 'Could not load the environment'); });
    return () => { live = false; };
  }, [project.id]);

  useEffect(() => {
    let live = true;
    api.sandboxes.get(project.sandbox_id)
      .then(sb => {
        // A failure here costs a hint, not the dialog: the environment is
        // still editable without knowing the image.
        if (!live) return;
        setSandbox((sb as { config?: SandboxSummary }).config || {});
        setSandboxType((sb as { type?: string }).type || '');
      })
      .catch(() => {});
    return () => { live = false; };
  }, [project.sandbox_id]);

  const changed = vars !== null && (containerEnv(vars) !== loaded || ports.trim() !== loadedPorts);
  const invalid = vars ? envError(vars) : null;
  const portList = parsePorts(ports);
  const portError = portList === null ? 'Ports must be numbers between 1 and 65535, separated by commas' : null;

  const save = async () => {
    if (!vars || saving) return;
    setSaving(true);
    try {
      await api.projects.update(project.id, {
        name: project.name, sandbox_id: project.sandbox_id,
        env: cleanEnv(vars), ports: portList ?? [], revision,
      });
      toast.success(changed ? 'Saved — the container is recreated on the next run' : 'Saved');
      onClose();
    } catch (e) {
      // 409: someone else edited this project since it was loaded.
      toast.error((e as Error).message || 'Could not save the project');
      setSaving(false);
    }
  };

  if (confirming) {
    const used = sessionCount && sessionCount > 0
      ? ` This project is used by ${sessionCount} session${sessionCount === 1 ? '' : 's'}.`
      : '';
    return (
      <Dialog
        title="Recreate the container?"
        onClose={() => setConfirming(false)}
        footerButtons={[
          { content: 'Back', onClick: () => setConfirming(false) },
          { content: saving ? 'Saving…' : 'Save', buttonType: 'primary', disabled: saving, onClick: () => { void save(); } },
        ]}
      >
        <p>
          The container for “{project.name}” is recreated at its <strong>next run</strong>.
        </p>
        <p className="env-editor-hint">
          Packages installed inside the container are lost; files under <code>/workspace</code> are not
          touched. Runs in flight and open terminals keep the container they started with.{used}
        </p>
      </Dialog>
    );
  }

  return (
    <Dialog
      title={`Settings — ${project.name}`}
      onClose={onClose}
      width="xlarge"
      footerButtons={[
        { content: 'Cancel', onClick: onClose },
        {
          content: saving ? 'Saving…' : 'Save',
          buttonType: 'primary',
          disabled: !vars || saving || !!invalid || !!portError,
          // Only a change to what the container gets is worth a warning; a
          // visibility toggle saves straight through.
          onClick: () => { if (changed) setConfirming(true); else void save(); },
        },
      ]}
    >
      {error && <Flash variant="danger">{error}</Flash>}
      {!error && !vars && <Spinner size="small" />}
      {vars && (
        <>
          <EnvEditor vars={vars} onChange={setVars} disabled={saving} />
          {invalid && <Flash variant="danger">{invalid}</Flash>}
          {/* Docker only: on an E2B-compatible service every port is already
              published, and there is nothing to declare. */}
          {sandboxType === 'docker' && (
            <FormControl>
              <FormControl.Label>Published ports</FormControl.Label>
              <TextInput
                block
                value={ports}
                disabled={saving}
                placeholder="3000, 5173"
                onChange={e => setPorts(e.target.value)}
              />
              <FormControl.Caption>
                The ports Preview can open, bound to this machine&apos;s loopback only. A server
                inside must listen on <code>0.0.0.0</code> — one bound to <code>127.0.0.1</code>
                {' '}is not reachable through a published port.
              </FormControl.Caption>
            </FormControl>
          )}
          {portError && <Flash variant="danger">{portError}</Flash>}
          {sandbox?.image && (
            <p className="env-editor-hint">
              Container image <code>{sandbox.image}</code>
              {sandbox.network ? '' : ' — this sandbox has no network, so nothing in the container can reach the internet'}
            </p>
          )}
        </>
      )}
    </Dialog>
  );
}
