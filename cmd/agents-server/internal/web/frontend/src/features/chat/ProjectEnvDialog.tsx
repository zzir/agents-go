import { useEffect, useState } from 'react';
import { Dialog, Flash, Spinner } from '@primer/react';
import type { ReactElement } from 'react';
import { api } from '@/lib/api';
import { toast } from '@/lib/toast';
import { EnvEditor, cleanEnv, envError } from '@/components/EnvEditor';
import type { EnvVar, Project, ProjectDetail } from '@/lib/binding';

/* The project's environment, loaded on open (a listing never carries one) and
   saved with the revision it was edited against. Values arrive masked and go
   back masked unless the person rewrites one.

   Saving a CHANGED environment replaces the container at the project's next
   run, so the confirm step spells out what that costs; a save that rewrote
   nothing goes through without asking. */

interface ProjectEnvDialogProps {
  project: Project;
  sessionCount?: number;
  onClose: () => void;
}

/* The master's settings, read-only: the image the container starts from and
   whether it has a network. Both answer questions this dialog provokes —
   "why can't the setup reach the internet?" above all — and both are the
   admin's to change, in Settings. */
interface SandboxSummary {
  image?: string;
  network?: boolean;
}

/* What the container is created with — the comparison the server makes to
   decide whether to replace it. Untouched rows compare equal because both
   sides still hold the mask. */
const containerEnv = (vars: EnvVar[]) =>
  JSON.stringify(cleanEnv(vars).map(v => [v.key, v.value]).sort((a, b) => a[0].localeCompare(b[0])));

export function ProjectEnvDialog({ project, sessionCount, onClose }: ProjectEnvDialogProps): ReactElement {
  const [vars, setVars] = useState<EnvVar[] | null>(null);
  const [sandbox, setSandbox] = useState<SandboxSummary | null>(null);
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
      })
      .catch((e: Error) => { if (live) setError(e.message || 'Could not load the environment'); });
    return () => { live = false; };
  }, [project.id]);

  useEffect(() => {
    let live = true;
    api.sandboxTemplates.get(project.template_id)
      .then(tpl => {
        // A failure here costs a hint, not the dialog: the environment is
        // still editable without knowing the image.
        if (live) setSandbox((tpl as { config?: SandboxSummary }).config || {});
      })
      .catch(() => {});
    return () => { live = false; };
  }, [project.template_id]);

  const changed = vars !== null && containerEnv(vars) !== loaded;
  const invalid = vars ? envError(vars) : null;

  const save = async () => {
    if (!vars || saving) return;
    setSaving(true);
    try {
      await api.projects.update(project.id, { name: project.name, template_id: project.template_id, env: cleanEnv(vars), revision });
      toast.success(changed ? 'Environment saved — the container is recreated on the next run' : 'Environment saved');
      onClose();
    } catch (e) {
      // 409: someone else edited this project since it was loaded.
      toast.error((e as Error).message || 'Could not save the environment');
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
      title={`Environment — ${project.name}`}
      onClose={onClose}
      width="xlarge"
      footerButtons={[
        { content: 'Cancel', onClick: onClose },
        {
          content: saving ? 'Saving…' : 'Save',
          buttonType: 'primary',
          disabled: !vars || saving || !!invalid,
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
