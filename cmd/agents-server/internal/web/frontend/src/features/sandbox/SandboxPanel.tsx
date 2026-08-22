import { useState } from 'react';
import { Button, TextInput, Label, Select, Checkbox, FormControl, Stack } from '@primer/react';
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

const TYPES = ['local', 'docker', 'ssh'] as const;

const TYPE_LABELS: Record<string, string> = { local: 'Local', docker: 'Docker', ssh: 'SSH' };

const TYPE_INFO: Record<string, string> = {
  local:  'Run code directly on the host machine.',
  docker: 'Run code inside an isolated Docker container.',
  ssh:    'Run code on a remote host over SSH (no isolation; resource limits are not enforced).',
};

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
  type: string;
  // docker
  image: string;
  runtime: string;
  network: boolean;
  persistent: boolean;
  host_dir: string;
  container_name: string;
  // ssh
  addr: string;
  user: string;
  key_file: string;
  password: string;
  use_agent: boolean;
  known_hosts: string;
  insecure_host_key: boolean;
  work_dir: string;
  // all backends; kept as a string so the field can be empty (= server default)
  max_read_file_bytes: string;
}

interface PackedForm {
  name: string;
  type: string;
  config?: Record<string, unknown>;
  revision?: number;
}

// The shape of the nested `config` blob per backend type. All fields are
// optional so one local cast covers any stored sandbox.
interface SandboxConfigShape {
  // all backends
  max_read_file_bytes?: number;
  // docker
  image?: string;
  runtime?: string;
  network?: boolean;
  persistent?: boolean;
  host_dir?: string;
  container_name?: string;
  // ssh
  addr?: string;
  user?: string;
  key_file?: string;
  password?: string;
  use_agent?: boolean;
  known_hosts?: string;
  insecure_host_key?: boolean;
  work_dir?: string;
}

// flatten turns a stored sandbox (top-level columns + nested config) into the
// flat field set the form edits. pack() is its inverse.
function flatten(s: Partial<SandboxConfig>): FlatForm {
  const c = (s.config || {}) as SandboxConfigShape;
  return {
    name: s.name || '', type: s.type || 'docker',
    // docker
    image: c.image || 'ghcr.io/zzir/sandbox:latest', runtime: c.runtime || '', network: !!c.network, persistent: !!c.persistent, host_dir: c.host_dir || '', container_name: c.container_name || '',
    // ssh
    addr: c.addr || '', user: c.user || '', key_file: c.key_file || '', password: c.password || '',
    use_agent: !!c.use_agent, known_hosts: c.known_hosts || '', insecure_host_key: !!c.insecure_host_key, work_dir: c.work_dir || '',
    max_read_file_bytes: c.max_read_file_bytes ? String(c.max_read_file_bytes) : '',
  };
}

// pack assembles the API payload: shared columns at the top level, backend
// settings under config (interpreted server-side per type).
function pack(form: FlatForm): PackedForm {
  const base: PackedForm = { name: form.name, type: form.type };
  let config: Record<string, unknown> | undefined;
  if (form.type === 'docker') {
    config = { image: form.image, runtime: form.runtime, user: form.user, network: form.network, persistent: form.persistent, host_dir: form.host_dir, container_name: form.container_name };
  } else if (form.type === 'ssh') {
    config = {
      addr: form.addr, user: form.user, use_agent: form.use_agent,
      key_file: form.key_file, password: form.password,
      known_hosts: form.known_hosts, insecure_host_key: form.insecure_host_key, work_dir: form.work_dir,
    };
  }
  // Shared across all backends (local included, which otherwise has no config).
  const maxRead = parseInt(form.max_read_file_bytes, 10);
  if (Number.isFinite(maxRead) && maxRead > 0) {
    config = { ...(config || {}), max_read_file_bytes: maxRead };
  }
  return config ? { ...base, config } : base;
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
  const t = form.type;

  return (
    <Stack gap="normal">
      {fc('Name', <TextInput block value={form.name} onChange={e => set('name', e.target.value)} placeholder="e.g. my-sandbox" />)}
      {fc('Type',
        <Select value={t} onChange={e => set('type', e.target.value)}>
          {TYPES.map(v => <Select.Option key={v} value={v}>{TYPE_LABELS[v]}</Select.Option>)}
        </Select>,
        TYPE_INFO[t],
      )}

      {t === 'docker' && fc('Runtime',
        <TextInput block value={form.runtime} onChange={e => set('runtime', e.target.value)} placeholder="runc" />,
        'OCI runtime. Use "runsc" for gVisor isolation. Leave empty for the daemon default (runc).',
      )}
      {t === 'docker' && fc('Image',
        <TextInput block value={form.image} onChange={e => set('image', e.target.value)} placeholder="ghcr.io/zzir/sandbox:latest" />,
      )}
      {t === 'docker' && fc('User',
        <TextInput block value={form.user} onChange={e => set('user', e.target.value)} placeholder="65534:65534" />,
        'user[:group] the container runs as. Leave empty for the non-privileged default (nobody). Use e.g. "root" or "1000:1000" when the workflow needs it.',
      )}
      {t === 'docker' && (
        <FormControl>
          <Checkbox checked={form.network} onChange={e => set('network', e.target.checked)} />
          <FormControl.Label>Allow network access</FormControl.Label>
        </FormControl>
      )}
      {t === 'docker' && (
        <FormControl>
          <Checkbox checked={form.persistent} onChange={e => set('persistent', e.target.checked)} />
          <FormControl.Label>Persistent container (reuse across executions)</FormControl.Label>
        </FormControl>
      )}
      {t === 'docker' && form.persistent && fc('Host directory',
        <TextInput block value={form.host_dir} onChange={e => set('host_dir', e.target.value)} placeholder="/path/on/host" />,
        'Host directory mounted at /workspace inside the container (commands always run in /workspace). Leave empty for the server --workspace.',
      )}
      {t === 'docker' && form.persistent && fc('Container name',
        <TextInput block value={form.container_name} onChange={e => set('container_name', e.target.value)} placeholder="e.g. sandbox-dev" />,
        'Docker container name. Leave empty for a random name.',
      )}

      {t === 'ssh' && fc('SSH host',
        <TextInput block value={form.addr} onChange={e => set('addr', e.target.value)} placeholder="dev-box:22" />,
        'Remote address as host or host:port (port defaults to 22).',
      )}
      {t === 'ssh' && fc('SSH user',
        <TextInput block value={form.user} onChange={e => set('user', e.target.value)} placeholder="sandbox" />,
      )}
      {t === 'ssh' && fc('Private key file',
        <TextInput block value={form.key_file} onChange={e => set('key_file', e.target.value)} placeholder="~/.ssh/id_ed25519" />,
        'Path on the server host. Tried before password. Leave empty to use a password or the SSH agent.',
      )}
      {t === 'ssh' && fc('Password',
        <SecretInput block value={form.password} onChange={e => set('password', e.target.value)} placeholder="(optional)" />,
      )}
      {t === 'ssh' && (
        <FormControl>
          <Checkbox checked={form.use_agent} onChange={e => set('use_agent', e.target.checked)} />
          <FormControl.Label>Use SSH agent (SSH_AUTH_SOCK)</FormControl.Label>
        </FormControl>
      )}
      {t === 'ssh' && fc('Known hosts file',
        <TextInput block value={form.known_hosts} onChange={e => set('known_hosts', e.target.value)} placeholder="~/.ssh/known_hosts" />,
        'Path on the server host. Empty uses the default ~/.ssh/known_hosts.',
      )}
      {t === 'ssh' && (
        <FormControl>
          <Checkbox checked={form.insecure_host_key} onChange={e => set('insecure_host_key', e.target.checked)} />
          <FormControl.Label>Skip host key verification (insecure -- dev/test only)</FormControl.Label>
        </FormControl>
      )}
      {t === 'ssh' && fc('Default directory',
        <TextInput block value={form.work_dir} onChange={e => set('work_dir', e.target.value)} placeholder="/home/sandbox/workspace" />,
        'The default project directory for new sessions — each session can pick its own before its first message. Leave empty for a temporary directory per execution.',
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
  if (s.type === 'docker') {
    if (c.image) parts.push(c.image);
  } else if (s.type === 'ssh') {
    parts.push((c.user ? c.user + '@' : '') + (c.addr || '?'));
    if (c.insecure_host_key) parts.push('insecure host key');
  }
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

  const typeLabel = (t: string) => TYPE_LABELS[t] || t;

  const form = adding ? <SandboxForm saving={saving} onSave={save} onCancel={cancel} />
    : editing ? <SandboxForm saving={saving} initial={editing} onSave={save} onCancel={cancel} onDelete={async () => { if (await remove(editing.id, editing.name)) cancel(); }} />
    : null;

  return (
    <CrudPanel title="Sandboxes" onAdd={startAdd} onCancel={cancel} form={form} isEmpty={items.length === 0} empty="No sandboxes configured.">
      {items.map(s => (
        <ResourceRow key={s.id}
          title={s.name}
          badges={<Label variant={BADGE.type}>{typeLabel(s.type)}</Label>}
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
