import { useState } from 'react';
import { Button, TextInput, Label, Checkbox, FormControl, Select, Stack } from '@primer/react';
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

// A sandbox is two rows: a TARGET says where it runs (and how to reach it),
// a TEMPLATE says what runs. A project pairs them — its target is fixed at
// creation, its template is editable. Two types: "docker" (a daemon on this
// machine or reachable over SSH) and "e2b" (any service speaking the E2B API
// — E2B's own, a self-hosted one, or a compatible one).

type SandboxType = 'docker' | 'e2b';

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

interface E2BTargetShape {
  api_url?: string;
  domain?: string;
  api_key?: string;
  data_plane_auth?: string;
}

interface TargetForm {
  name: string;
  type: SandboxType;
  host: string;
  ssh_key_file: string;
  ssh_password: string;
  ssh_use_agent: boolean;
  ssh_known_hosts: string;
  ssh_insecure_host_key: boolean;
  api_url: string;
  domain: string;
  api_key: string;
  data_plane_auth: string;
}

function flattenTarget(s: Partial<SandboxRow>): TargetForm {
  const c = (s.config || {}) as TargetShape & E2BTargetShape;
  return {
    name: s.name || '',
    type: (s.type as SandboxType) || 'docker',
    host: c.host || '',
    ssh_key_file: c.ssh_key_file || '', ssh_password: c.ssh_password || '',
    ssh_use_agent: !!c.ssh_use_agent, ssh_known_hosts: c.ssh_known_hosts || '',
    ssh_insecure_host_key: !!c.ssh_insecure_host_key,
    api_url: c.api_url || '', domain: c.domain || '', api_key: c.api_key || '',
    data_plane_auth: c.data_plane_auth || '',
  };
}

function packTarget(form: TargetForm): PackedForm {
  if (form.type === 'e2b') {
    return {
      name: form.name, type: 'e2b',
      config: {
        api_url: form.api_url, domain: form.domain,
        api_key: form.api_key, data_plane_auth: form.data_plane_auth,
      },
    };
  }
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
  const remote = form.type === 'docker' && form.host.startsWith('ssh://');

  return (
    <Stack gap="normal">
      {fc('Name', <TextInput block value={form.name} onChange={e => set('name', e.target.value)} placeholder="e.g. laptop" />)}
      {fc('Type', (
        // Frozen on an existing target: the type is its identity, and the
        // server refuses the change while projects live on it.
        <Select block value={form.type} disabled={!!initial} onChange={e => set('type', e.target.value as SandboxType)}>
          <Select.Option value="docker">Docker daemon</Select.Option>
          <Select.Option value="e2b">E2B-compatible service</Select.Option>
        </Select>
      ), 'Where sandboxes run. Frozen once the target exists.')}
      {form.type === 'e2b' && fc('API URL',
        <TextInput block value={form.api_url} onChange={e => set('api_url', e.target.value)} placeholder="https://api.e2b.app" />,
        'The control plane. Empty uses E2B\'s own; a compatible service (Alibaba Cloud Function Compute, a self-hosted E2B) has its own.',
      )}
      {form.type === 'e2b' && fc('Sandbox domain',
        <TextInput block value={form.domain} onChange={e => set('domain', e.target.value)} placeholder="e2b.app" />,
        'The suffix a sandbox\'s public hosts are built from: <port>-<sandbox id>.<domain>.',
      )}
      {form.type === 'e2b' && fc('API key',
        <SecretInput block value={form.api_key} onChange={e => set('api_key', e.target.value)} placeholder="e2b_…" />,
      )}
      {form.type === 'e2b' && fc('Daemon credential', (
        <Select block value={form.data_plane_auth} onChange={e => set('data_plane_auth', e.target.value)}>
          <Select.Option value="">Automatic</Select.Option>
          <Select.Option value="access_token">Per-sandbox access token</Select.Option>
          <Select.Option value="api_key">API key</Select.Option>
          <Select.Option value="none">None</Select.Option>
        </Select>
      ), 'What the in-sandbox daemon is authenticated with. Automatic uses the token the service mints, and the API key when it mints none — which is what a compatible service without token support needs.')}
      {form.type === 'docker' && fc('Daemon',
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
  if (s.type === 'e2b') {
    const c = (s.config || {}) as E2BTargetShape;
    return [c.api_url || 'api.e2b.app', c.domain || 'e2b.app'].join(' · ');
  }
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
          badges={<Label variant={BADGE.type}>{targetBadge(s)}</Label>}
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

// targetBadge names the kind at a glance: which service, or which daemon.
function targetBadge(s: SandboxRow): string {
  if (s.type === 'e2b') return 'E2B';
  return (s.config as TargetShape)?.host ? 'Remote' : 'Local';
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

interface E2BTemplateShape {
  template_id?: string;
  timeout_seconds?: number;
  auto_pause?: boolean;
  allow_internet?: boolean;
  max_read_file_bytes?: number;
}

interface TemplateFormState {
  name: string;
  type: SandboxType;
  image: string;
  runtime: string;
  user: string;
  network: string;
  template_id: string;
  timeout_seconds: string;
  auto_pause: boolean;
  allow_internet: boolean;
  // numbers kept as strings so the fields can be empty (= server default)
  memory_mb: string;
  cpus: string;
  max_read_file_bytes: string;
}

function flattenTemplate(s: Partial<SandboxRow>): TemplateFormState {
  const c = (s.config || {}) as TemplateShape & E2BTemplateShape;
  const type = (s.type as SandboxType) || 'docker';
  return {
    name: s.name || '',
    type,
    image: c.image || (type === 'docker' ? 'ghcr.io/zzir/sandbox:latest' : ''),
    runtime: c.runtime || '', user: c.user || '', network: c.network || '',
    template_id: c.template_id || '',
    timeout_seconds: c.timeout_seconds ? String(c.timeout_seconds) : '',
    auto_pause: c.auto_pause ?? true,
    allow_internet: !!c.allow_internet,
    memory_mb: c.memory_mb ? String(c.memory_mb) : '',
    cpus: c.cpus ? String(c.cpus) : '',
    max_read_file_bytes: c.max_read_file_bytes ? String(c.max_read_file_bytes) : '',
  };
}

function packTemplate(form: TemplateFormState): PackedForm {
  const maxRead = parseInt(form.max_read_file_bytes, 10);
  if (form.type === 'e2b') {
    const config: Record<string, unknown> = {
      template_id: form.template_id,
      auto_pause: form.auto_pause,
      allow_internet: form.allow_internet,
    };
    const timeout = parseInt(form.timeout_seconds, 10);
    if (Number.isFinite(timeout) && timeout > 0) config.timeout_seconds = timeout;
    if (Number.isFinite(maxRead) && maxRead > 0) config.max_read_file_bytes = maxRead;
    return { name: form.name, type: 'e2b', config };
  }
  const config: Record<string, unknown> = {
    image: form.image, runtime: form.runtime, user: form.user, network: form.network,
  };
  const memory = parseInt(form.memory_mb, 10);
  if (Number.isFinite(memory) && memory > 0) config.memory_mb = memory;
  const cpus = parseFloat(form.cpus);
  if (Number.isFinite(cpus) && cpus > 0) config.cpus = cpus;
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
      {fc('Type', (
        // Immutable on an existing template: a docker template cannot become
        // one its machine could not run, and a second template costs nothing.
        <Select block value={form.type} disabled={!!initial} onChange={e => set('type', e.target.value as SandboxType)}>
          <Select.Option value="docker">Docker image</Select.Option>
          <Select.Option value="e2b">E2B template</Select.Option>
        </Select>
      ), 'Must match the target it is used on. Immutable.')}
      {form.type === 'e2b' && fc('Template id',
        <TextInput block value={form.template_id} onChange={e => set('template_id', e.target.value)} placeholder="base" />,
        'A template that already exists on the service — the workbench builds none. Its console or CLI is where they are made.',
      )}
      {form.type === 'e2b' && fc('Lease (seconds)',
        <TextInput block type="number" value={form.timeout_seconds} onChange={e => set('timeout_seconds', e.target.value)} placeholder="300" />,
        'How long a sandbox lives before the service acts on it. Refreshed while in use.',
      )}
      {form.type === 'e2b' && (
        <FormControl>
          <Checkbox checked={form.auto_pause} onChange={e => set('auto_pause', e.target.checked)} />
          <FormControl.Label>Pause on expiry instead of killing</FormControl.Label>
          <FormControl.Caption>Off means an expired lease DESTROYS the working tree.</FormControl.Caption>
        </FormControl>
      )}
      {form.type === 'e2b' && (
        <FormControl>
          <Checkbox checked={form.allow_internet} onChange={e => set('allow_internet', e.target.checked)} />
          <FormControl.Label>Allow outbound network access</FormControl.Label>
        </FormControl>
      )}
      {form.type === 'docker' && fc('Image',
        <TextInput block value={form.image} onChange={e => set('image', e.target.value)} placeholder="ghcr.io/zzir/sandbox:latest" />,
      )}
      {form.type === 'docker' && fc('Runtime',
        <TextInput block value={form.runtime} onChange={e => set('runtime', e.target.value)} placeholder="runc" />,
        'OCI runtime. Use "runsc" for gVisor isolation. Leave empty for the daemon default (runc). Whether it exists is up to the target machine.',
      )}
      {form.type === 'docker' && fc('User',
        <TextInput block value={form.user} onChange={e => set('user', e.target.value)} placeholder="root" />,
        'user[:group] the container runs as. Empty runs as root, so the agent can install packages into its own container; the working tree lives in a volume nobody else mounts.',
      )}
      {form.type === 'docker' && fc('Network',
        <TextInput block value={form.network} onChange={e => set('network', e.target.value)} placeholder="(none)" />,
        'The Docker network the container joins. Empty = no network at all. "bridge" gives ordinary networking; a user-defined network name puts it where the server can reach it.',
      )}
      {form.type === 'docker' && fc('Memory limit (MB)',
        <TextInput block type="number" value={form.memory_mb} onChange={e => set('memory_mb', e.target.value)} placeholder="unlimited" />,
        'Hard memory cap per container. Empty = unlimited.',
      )}
      {form.type === 'docker' && fc('CPU limit',
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
  if (s.type === 'e2b') {
    const c = (s.config || {}) as E2BTemplateShape;
    const parts = [c.template_id || '(no template id)'];
    parts.push(c.auto_pause === false ? 'killed on expiry' : 'paused on expiry');
    if (c.allow_internet) parts.push('internet');
    return parts.join(' · ');
  }
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
          badges={<Label variant={BADGE.type}>{s.type === 'e2b' ? 'E2B' : 'Docker'}</Label>}
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
