import { useState } from 'react';
import { Button, TextInput, Label, Checkbox, FormControl, Stack } from '@primer/react';
import { SecretInput } from '@/components/SecretInput';
import { FormActions } from '@/components/FormActions';
import { CrudPanel, RowActionsMenu } from '@/components/CrudPanel';
import { useReadOnly } from '@/lib/access';
import { ResourceRow } from '@/components/ResourceRow';
import { api } from '@/lib/api';
import { BADGE } from '@/lib/badges';
import { useApi, useCrud } from '@/lib/hooks';
import { fc } from '@/lib/form';
import { toast } from '@/lib/toast';

// A sandbox is two rows: a TARGET says which machine runs it (and how to
// reach it), a TEMPLATE says what runs. A project pairs them — its target is
// fixed at creation, its template is editable.

interface SandboxRow {
  id: string;
  name: string;
  type: string;
  config: Record<string, unknown>;
  // Row version from the server; sent back on save so an edit made on a
  // stale form is refused (409) instead of overwriting a concurrent update.
  revision?: number;
}

interface TestResult {
  ok: boolean;
  detail?: string;
}

interface PackedForm {
  name: string;
  type: string;
  config?: Record<string, unknown>;
  revision?: number;
}

/* ---------- targets ---------- */

interface TargetShape {
  host?: string;
  ssh_key_file?: string;
  ssh_password?: string;
  ssh_use_agent?: boolean;
  ssh_known_hosts?: string;
  ssh_insecure_host_key?: boolean;
}

interface TargetForm {
  name: string;
  host: string;
  ssh_key_file: string;
  ssh_password: string;
  ssh_use_agent: boolean;
  ssh_known_hosts: string;
  ssh_insecure_host_key: boolean;
}

function flattenTarget(s: Partial<SandboxRow>): TargetForm {
  const c = (s.config || {}) as TargetShape;
  return {
    name: s.name || '',
    host: c.host || '',
    ssh_key_file: c.ssh_key_file || '', ssh_password: c.ssh_password || '',
    ssh_use_agent: !!c.ssh_use_agent, ssh_known_hosts: c.ssh_known_hosts || '',
    ssh_insecure_host_key: !!c.ssh_insecure_host_key,
  };
}

function packTarget(form: TargetForm): PackedForm {
  const config: Record<string, unknown> = { host: form.host };
  if (form.host.startsWith('ssh://')) {
    config.ssh_use_agent = form.ssh_use_agent;
    config.ssh_key_file = form.ssh_key_file;
    config.ssh_password = form.ssh_password;
    config.ssh_known_hosts = form.ssh_known_hosts;
    config.ssh_insecure_host_key = form.ssh_insecure_host_key;
  }
  return { name: form.name, type: 'docker', config };
}

function TargetForm({ initial, onSave, onCancel, onDelete, saving }: {
  initial?: SandboxRow;
  onSave: (form: PackedForm) => void;
  onCancel?: () => void;
  onDelete?: () => void;
  saving?: boolean;
}) {
  const [form, setForm] = useState<TargetForm>(flattenTarget(initial ?? { name: '' }));
  const set = (k: keyof TargetForm, v: unknown) => setForm(prev => ({ ...prev, [k]: v }));
  const remote = form.host.startsWith('ssh://');

  return (
    <Stack gap="normal">
      {fc('Name', <TextInput block value={form.name} onChange={e => set('name', e.target.value)} placeholder="e.g. laptop" />)}
      {fc('Daemon',
        <TextInput block value={form.host} onChange={e => set('host', e.target.value)} placeholder="ssh://user@host — empty for the local daemon" />,
        'Where the containers run: empty = this machine\'s Docker daemon; ssh://user@host reaches a remote daemon over SSH; tcp://host:port a TCP-exposed one. Frozen once projects live on it.',
      )}
      {remote && fc('SSH private key file',
        <TextInput block value={form.ssh_key_file} onChange={e => set('ssh_key_file', e.target.value)} placeholder="~/.ssh/id_ed25519" />,
        'Path on the server host. Tried before password. Leave empty to use a password or the SSH agent.',
      )}
      {remote && fc('SSH password',
        <SecretInput block value={form.ssh_password} onChange={e => set('ssh_password', e.target.value)} placeholder="(optional)" />,
      )}
      {remote && (
        <FormControl>
          <Checkbox checked={form.ssh_use_agent} onChange={e => set('ssh_use_agent', e.target.checked)} />
          <FormControl.Label>Use SSH agent (SSH_AUTH_SOCK)</FormControl.Label>
        </FormControl>
      )}
      {remote && fc('SSH known hosts file',
        <TextInput block value={form.ssh_known_hosts} onChange={e => set('ssh_known_hosts', e.target.value)} placeholder="~/.ssh/known_hosts" />,
        'Path on the server host. Empty uses the default ~/.ssh/known_hosts.',
      )}
      {remote && (
        <FormControl>
          <Checkbox checked={form.ssh_insecure_host_key} onChange={e => set('ssh_insecure_host_key', e.target.checked)} />
          <FormControl.Label>Skip host key verification (insecure -- dev/test only)</FormControl.Label>
        </FormControl>
      )}
      <FormActions saving={saving} onSave={() => onSave({ ...packTarget(form), revision: initial?.revision })} onCancel={onCancel} onDelete={onDelete} />
    </Stack>
  );
}

function targetSummary(s: SandboxRow): string {
  const c = (s.config || {}) as TargetShape;
  const parts: string[] = [c.host ? c.host.replace(/^ssh:\/\//, '') : 'local daemon'];
  if (c.ssh_insecure_host_key) parts.push('insecure host key');
  return parts.join(' · ');
}

export function SandboxTargetsPanel() {
  const readOnly = useReadOnly();
  const { items, adding, editing, startAdd, startEdit, cancel, save, saving, remove } =
    useCrud<SandboxRow, PackedForm>(api.sandboxTargets);
  // The health check needs an image, which a target does not carry: it
  // borrows the first template.
  const { data: templates } = useApi<SandboxRow[]>(() => api.sandboxTemplates.list() as unknown as Promise<SandboxRow[]>);
  const [testingId, setTestingId] = useState<string | null>(null);

  const handleTest = async (s: SandboxRow) => {
    const tpl = (templates || [])[0];
    if (!tpl) {
      toast.error('Add a template first — a health check needs an image to run');
      return;
    }
    setTestingId(s.id);
    try {
      const res = await api.sandboxTargets.test(s.id, tpl.id) as TestResult;
      if (res.ok) {
        toast.success('Target "' + s.name + '" is working');
      } else {
        toast.error('Target "' + s.name + '" failed: ' + (res.detail || 'unknown error'));
      }
    } catch (e) {
      toast.error((e as Error).message || 'Test failed');
    } finally {
      setTestingId(null);
    }
  };

  const form = adding ? <TargetForm saving={saving} onSave={save} onCancel={cancel} />
    : editing ? <TargetForm saving={saving} initial={editing} onSave={save} onCancel={cancel} onDelete={async () => { if (await remove(editing.id, editing.name)) cancel(); }} />
    : null;

  return (
    <CrudPanel title="Sandbox targets" onAdd={startAdd} onCancel={cancel} form={form} isEmpty={items.length === 0} empty="No sandbox targets configured.">
      {items.map(s => (
        <ResourceRow key={s.id}
          title={s.name}
          badges={<Label variant={BADGE.type}>{(s.config as TargetShape)?.host ? 'Remote' : 'Local'}</Label>}
          sub={targetSummary(s)}
          actions={<>
            {!readOnly && (
              <Button onClick={() => handleTest(s)} size="small" disabled={testingId === s.id} style={{ color: 'var(--fgColor-success)' }}>
                {testingId === s.id ? 'Testing...' : 'Test'}
              </Button>
            )}
            <RowActionsMenu name={s.name} onEdit={() => startEdit(s)} />
          </>}
        />
      ))}
    </CrudPanel>
  );
}

/* ---------- templates ---------- */

interface TemplateShape {
  image?: string;
  runtime?: string;
  user?: string;
  network?: string;
  memory_mb?: number;
  cpus?: number;
  max_read_file_bytes?: number;
}

interface TemplateFormState {
  name: string;
  image: string;
  runtime: string;
  user: string;
  network: string;
  // numbers kept as strings so the fields can be empty (= server default)
  memory_mb: string;
  cpus: string;
  max_read_file_bytes: string;
}

function flattenTemplate(s: Partial<SandboxRow>): TemplateFormState {
  const c = (s.config || {}) as TemplateShape;
  return {
    name: s.name || '',
    image: c.image || 'ghcr.io/zzir/sandbox:latest',
    runtime: c.runtime || '', user: c.user || '', network: c.network || '',
    memory_mb: c.memory_mb ? String(c.memory_mb) : '',
    cpus: c.cpus ? String(c.cpus) : '',
    max_read_file_bytes: c.max_read_file_bytes ? String(c.max_read_file_bytes) : '',
  };
}

function packTemplate(form: TemplateFormState): PackedForm {
  const config: Record<string, unknown> = {
    image: form.image, runtime: form.runtime, user: form.user, network: form.network,
  };
  const memory = parseInt(form.memory_mb, 10);
  if (Number.isFinite(memory) && memory > 0) config.memory_mb = memory;
  const cpus = parseFloat(form.cpus);
  if (Number.isFinite(cpus) && cpus > 0) config.cpus = cpus;
  const maxRead = parseInt(form.max_read_file_bytes, 10);
  if (Number.isFinite(maxRead) && maxRead > 0) config.max_read_file_bytes = maxRead;
  return { name: form.name, type: 'docker', config };
}

function TemplateForm({ initial, onSave, onCancel, onDelete, saving }: {
  initial?: SandboxRow;
  onSave: (form: PackedForm) => void;
  onCancel?: () => void;
  onDelete?: () => void;
  saving?: boolean;
}) {
  const [form, setForm] = useState<TemplateFormState>(flattenTemplate(initial ?? { name: '' }));
  const set = (k: keyof TemplateFormState, v: unknown) => setForm(prev => ({ ...prev, [k]: v }));

  return (
    <Stack gap="normal">
      {fc('Name', <TextInput block value={form.name} onChange={e => set('name', e.target.value)} placeholder="e.g. python-3.12" />)}
      {fc('Image',
        <TextInput block value={form.image} onChange={e => set('image', e.target.value)} placeholder="ghcr.io/zzir/sandbox:latest" />,
      )}
      {fc('Runtime',
        <TextInput block value={form.runtime} onChange={e => set('runtime', e.target.value)} placeholder="runc" />,
        'OCI runtime. Use "runsc" for gVisor isolation. Leave empty for the daemon default (runc). Whether it exists is up to the target machine.',
      )}
      {fc('User',
        <TextInput block value={form.user} onChange={e => set('user', e.target.value)} placeholder="root" />,
        'user[:group] the container runs as. Empty runs as root, so the agent can install packages into its own container; the working tree lives in a volume nobody else mounts.',
      )}
      {fc('Network',
        <TextInput block value={form.network} onChange={e => set('network', e.target.value)} placeholder="(none)" />,
        'The Docker network the container joins. Empty = no network at all. "bridge" gives ordinary networking; a user-defined network name puts it where the server can reach it.',
      )}
      {fc('Memory limit (MB)',
        <TextInput block type="number" value={form.memory_mb} onChange={e => set('memory_mb', e.target.value)} placeholder="unlimited" />,
        'Hard memory cap per container. Empty = unlimited.',
      )}
      {fc('CPU limit',
        <TextInput block type="number" value={form.cpus} onChange={e => set('cpus', e.target.value)} placeholder="daemon default" />,
        'CPU cores per container (fractional allowed, e.g. 0.5). Empty = the daemon default.',
      )}
      {fc('Max read_file bytes',
        <TextInput block type="number" value={form.max_read_file_bytes} onChange={e => set('max_read_file_bytes', e.target.value)} placeholder="8388608" />,
        'Cap on bytes a single read_file returns; larger files fail instead of loading into memory. Empty = 8 MiB default.',
      )}
      <FormActions saving={saving} onSave={() => onSave({ ...packTemplate(form), revision: initial?.revision })} onCancel={onCancel} onDelete={onDelete} />
    </Stack>
  );
}

function templateSummary(s: SandboxRow): string {
  const c = (s.config || {}) as TemplateShape;
  const parts: string[] = [];
  if (c.image) parts.push(c.image);
  if (c.runtime) parts.push(c.runtime);
  parts.push(c.network ? `network ${c.network}` : 'no network');
  return parts.join(' · ');
}

export function SandboxTemplatesPanel() {
  const { items, adding, editing, startAdd, startEdit, cancel, save, saving, remove } =
    useCrud<SandboxRow, PackedForm>(api.sandboxTemplates);

  const form = adding ? <TemplateForm saving={saving} onSave={save} onCancel={cancel} />
    : editing ? <TemplateForm saving={saving} initial={editing} onSave={save} onCancel={cancel} onDelete={async () => { if (await remove(editing.id, editing.name)) cancel(); }} />
    : null;

  return (
    <CrudPanel title="Sandbox templates" onAdd={startAdd} onCancel={cancel} form={form} isEmpty={items.length === 0} empty="No sandbox templates configured.">
      {items.map(s => (
        <ResourceRow key={s.id}
          title={s.name}
          badges={<Label variant={BADGE.type}>Docker</Label>}
          sub={templateSummary(s)}
          actions={<RowActionsMenu name={s.name} onEdit={() => startEdit(s)} />}
        />
      ))}
    </CrudPanel>
  );
}

// The settings entry shows both: the machines, then what runs on them.
export function SandboxPanel() {
  return (
    <Stack gap="normal">
      <SandboxTargetsPanel />
      <SandboxTemplatesPanel />
    </Stack>
  );
}

export default SandboxPanel;
