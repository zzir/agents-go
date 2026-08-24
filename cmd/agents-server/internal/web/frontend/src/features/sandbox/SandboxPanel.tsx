import { useState } from 'react';
import { Button, TextInput, Label, Checkbox, FormControl, Stack } from '@primer/react';
import { SecretInput } from '@/components/SecretInput';
import { FormActions } from '@/components/FormActions';
import { CrudPanel, RowEditButton } from '@/components/CrudPanel';
import { useReadOnly } from '@/lib/access';
import { ResourceRow } from '@/components/ResourceRow';
import { api } from '@/lib/api';
import { BADGE } from '@/lib/badges';
import { useCrud } from '@/lib/hooks';
import { fc } from '@/lib/form';
import { toast } from '@/lib/toast';

// Every sandbox is a Docker container; a remote daemon is reached over SSH
// via config.host.

interface SandboxConfig {
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

interface FlatForm {
  name: string;
  image: string;
  host: string;
  runtime: string;
  user: string;
  network: boolean;
  // SSH auth for an ssh:// host
  ssh_key_file: string;
  ssh_password: string;
  ssh_use_agent: boolean;
  ssh_known_hosts: string;
  ssh_insecure_host_key: boolean;
  // numbers kept as strings so the fields can be empty (= server default)
  memory_mb: string;
  cpus: string;
  max_read_file_bytes: string;
}

interface PackedForm {
  name: string;
  type: string;
  config?: Record<string, unknown>;
  revision?: number;
}

// The shape of the nested `config` blob. All fields are optional so one cast
// covers any stored sandbox.
interface SandboxConfigShape {
  image?: string;
  host?: string;
  runtime?: string;
  user?: string;
  network?: boolean;
  ssh_key_file?: string;
  ssh_password?: string;
  ssh_use_agent?: boolean;
  ssh_known_hosts?: string;
  ssh_insecure_host_key?: boolean;
  memory_mb?: number;
  cpus?: number;
  max_read_file_bytes?: number;
}

// flatten turns a stored sandbox (top-level columns + nested config) into the
// flat field set the form edits. pack() is its inverse.
function flatten(s: Partial<SandboxConfig>): FlatForm {
  const c = (s.config || {}) as SandboxConfigShape;
  return {
    name: s.name || '',
    image: c.image || 'ghcr.io/zzir/sandbox:latest', host: c.host || '', runtime: c.runtime || '',
    user: c.user || '', network: !!c.network,
    ssh_key_file: c.ssh_key_file || '', ssh_password: c.ssh_password || '',
    ssh_use_agent: !!c.ssh_use_agent, ssh_known_hosts: c.ssh_known_hosts || '',
    ssh_insecure_host_key: !!c.ssh_insecure_host_key,
    memory_mb: c.memory_mb ? String(c.memory_mb) : '',
    cpus: c.cpus ? String(c.cpus) : '',
    max_read_file_bytes: c.max_read_file_bytes ? String(c.max_read_file_bytes) : '',
  };
}

// pack assembles the API payload: shared columns at the top level, backend
// settings under config (interpreted server-side per type).
function pack(form: FlatForm): PackedForm {
  const config: Record<string, unknown> = {
    image: form.image, host: form.host, runtime: form.runtime, user: form.user,
    network: form.network,
  };
  if (form.host.startsWith('ssh://')) {
    config.ssh_use_agent = form.ssh_use_agent;
    config.ssh_key_file = form.ssh_key_file;
    config.ssh_password = form.ssh_password;
    config.ssh_known_hosts = form.ssh_known_hosts;
    config.ssh_insecure_host_key = form.ssh_insecure_host_key;
  }
  const memory = parseInt(form.memory_mb, 10);
  if (Number.isFinite(memory) && memory > 0) config.memory_mb = memory;
  const cpus = parseFloat(form.cpus);
  if (Number.isFinite(cpus) && cpus > 0) config.cpus = cpus;
  const maxRead = parseInt(form.max_read_file_bytes, 10);
  if (Number.isFinite(maxRead) && maxRead > 0) config.max_read_file_bytes = maxRead;
  return { name: form.name, type: 'docker', config };
}

interface SandboxFormProps {
  initial?: SandboxConfig;
  onSave: (form: PackedForm) => void;
  onCancel?: () => void;
  onDelete?: () => void;
  saving?: boolean;
}

function SandboxForm({ initial, onSave, onCancel, onDelete, saving }: SandboxFormProps) {
  const blank: Partial<SandboxConfig> = { name: '', type: 'docker' };
  const [form, setForm] = useState<FlatForm>(initial ? flatten(initial) : flatten(blank));
  const set = (k: keyof FlatForm, v: unknown) => setForm(prev => ({ ...prev, [k]: v }));
  const remote = form.host.startsWith('ssh://');

  return (
    <Stack gap="normal">
      {fc('Name', <TextInput block value={form.name} onChange={e => set('name', e.target.value)} placeholder="e.g. my-sandbox" />)}
      {fc('Image',
        <TextInput block value={form.image} onChange={e => set('image', e.target.value)} placeholder="ghcr.io/zzir/sandbox:latest" />,
      )}
      {fc('Daemon',
        <TextInput block value={form.host} onChange={e => set('host', e.target.value)} placeholder="ssh://user@host — empty for the local daemon" />,
        'Where the containers run: empty = this machine\'s Docker daemon; ssh://user@host reaches a remote daemon over SSH; tcp://host:port a TCP-exposed one.',
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
      {fc('Runtime',
        <TextInput block value={form.runtime} onChange={e => set('runtime', e.target.value)} placeholder="runc" />,
        'OCI runtime. Use "runsc" for gVisor isolation. Leave empty for the daemon default (runc).',
      )}
      {fc('User',
        <TextInput block value={form.user} onChange={e => set('user', e.target.value)} placeholder="65534:65534" />,
        'user[:group] the container runs as. Leave empty for the non-privileged default (nobody). Use e.g. "root" or "1000:1000" when the workflow needs it.',
      )}
      <FormControl>
        <Checkbox checked={form.network} onChange={e => set('network', e.target.checked)} />
        <FormControl.Label>Allow network access</FormControl.Label>
      </FormControl>
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

      <FormActions saving={saving} onSave={() => onSave({ ...pack(form), revision: initial?.revision })} onCancel={onCancel} onDelete={onDelete} />
    </Stack>
  );
}

function sandboxSummary(s: SandboxConfig): string {
  const c = (s.config || {}) as SandboxConfigShape;
  const parts: string[] = [];
  if (c.image) parts.push(c.image);
  if (c.host) parts.push(c.host.replace(/^ssh:\/\//, ''));
  if (c.ssh_insecure_host_key) parts.push('insecure host key');
  return parts.join(' · ') || 'default settings';
}

export function SandboxPanel() {
  const readOnly = useReadOnly();
  const { items, adding, editing, startAdd, startEdit, cancel, save, saving, remove } =
    useCrud<SandboxConfig, PackedForm>(api.sandboxes);
  const [testingId, setTestingId] = useState<string | null>(null);

  const handleTest = async (s: SandboxConfig) => {
    setTestingId(s.id);
    try {
      const res = await api.sandboxes.test(s.id) as TestResult;
      if (res.ok) {
        toast.success('Sandbox "' + s.name + '" is working');
      } else {
        toast.error('Sandbox "' + s.name + '" failed: ' + (res.detail || 'unknown error'));
      }
    } catch (e) {
      toast.error((e as Error).message || 'Test failed');
    } finally {
      setTestingId(null);
    }
  };

  const form = adding ? <SandboxForm saving={saving} onSave={save} onCancel={cancel} />
    : editing ? <SandboxForm saving={saving} initial={editing} onSave={save} onCancel={cancel} onDelete={async () => { if (await remove(editing.id, editing.name)) cancel(); }} />
    : null;

  return (
    <CrudPanel title="Sandboxes" onAdd={startAdd} onCancel={cancel} form={form} isEmpty={items.length === 0} empty="No sandboxes configured.">
      {items.map(s => (
        <ResourceRow key={s.id}
          title={s.name}
          badges={<Label variant={BADGE.type}>{(s.config as SandboxConfigShape)?.host ? 'Remote' : 'Docker'}</Label>}
          sub={sandboxSummary(s)}
          actions={<>
            {!readOnly && (
              <Button onClick={() => handleTest(s)} size="small" disabled={testingId === s.id} style={{ color: 'var(--fgColor-success)' }}>
                {testingId === s.id ? 'Testing...' : 'Test'}
              </Button>
            )}
            <RowEditButton onClick={() => startEdit(s)} />
          </>}
        />
      ))}
    </CrudPanel>
  );
}

export default SandboxPanel;
