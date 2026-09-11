import { useState } from 'react';
import { TextInput, Textarea, Stack, Select } from '@primer/react';
import { AgentPicker } from '@/components/AgentPicker';
import { FormActions } from '@/components/FormActions';
import { CrudPanel, RowActionsMenu } from '@/components/CrudPanel';
import { ResourceRow } from '@/components/ResourceRow';
import { api } from '@/lib/api';
import { useApi, useCrud } from '@/lib/hooks';
import { fc } from '@/lib/form';
import { JsonField } from '@/lib/JsonField';

// A configuration memory: global (every agent reads it) or an agent's. A
// session's own memory lives with the session, in the Context panel.
interface Memory {
  id: string;
  scope_kind: string;
  scope_id?: string;
  key: string;
  content: string;
  metadata?: string;
  written_by: string;
  created_at: string;
  updated_at: string;
}

interface AgentConfig {
  id: string;
  name: string;
  avatar?: string;
  scope?: string;
}

interface MemoryFormData {
  scope_kind: string;
  scope_id: string;
  key: string;
  content: string;
  metadata: string;
}

interface MemoryFormProps {
  initial: MemoryFormData;
  // An edit keeps its scope and key: they identify the memory.
  locked?: boolean;
  onSave: (form: MemoryFormData) => void;
  onCancel?: (() => void) | null;
  onDelete?: (() => void) | null;
  saving?: boolean;
  agents: AgentConfig[] | null;
}

function MemoryForm({ initial, locked, onSave, onCancel, onDelete, saving, agents }: MemoryFormProps) {
  const [form, setForm] = useState<MemoryFormData>(initial);
  const set = (k: keyof MemoryFormData, v: string) =>
    setForm(prev => ({ ...prev, [k]: v }));

  return (
    <Stack gap="normal">
      {fc(
        'Scope',
        <Select block value={form.scope_kind} disabled={locked}
          onChange={e => setForm(prev => ({ ...prev, scope_kind: e.target.value, scope_id: e.target.value === 'global' ? '' : prev.scope_id }))}>
          <Select.Option value="global">Global — every agent</Select.Option>
          <Select.Option value="agent">One agent</Select.Option>
        </Select>,
        'Global memory is an admin\'s to write; an agent\'s memory is its editor\'s',
      )}
      {form.scope_kind === 'agent' && fc(
        'Agent',
        <AgentPicker
          agents={agents || []}
          value={form.scope_id || ''}
          onChange={id => set('scope_id', id)}
          emptyLabel="(choose an agent)"
        />,
      )}
      {fc(
        'Key',
        <TextInput block
          value={form.key}
          disabled={locked}
          onChange={e => set('key', e.target.value)}
          placeholder="unique-key"
        />,
      )}
      {fc(
        'Content',
        <Textarea block
          value={form.content}
          onChange={e => set('content', e.target.value)}
          rows={4}
        />,
      )}
      <JsonField
        label="Metadata (JSON)"
        value={form.metadata}
        onChange={v => set('metadata', v)}
        placeholder='{"tag": "value"}'
      />
      <FormActions saving={saving} onSave={() => onSave(form)} onCancel={onCancel} onDelete={onDelete} />
    </Stack>
  );
}

// The picker value for the rows of agents since deleted, offered only while
// any exist, for the admin to clear; no agent id looks like it.
const DELETED = 'deleted';

// The list shows one scope at a time: global (the default), or one agent's.
export function MemoryPanel() {
  const { items: memories, loading, adding, editing, startAdd, startEdit, cancel, save, saving, remove } =
    useCrud<Memory, MemoryFormData>(api.memories, 'memories');
  const { data: agents } = useApi<AgentConfig[]>(() => api.agents.list() as Promise<AgentConfig[]>, [], 'agents');
  const [scope, setScope] = useState('');

  const known = new Set((agents || []).map(a => a.id));
  const orphans = agents ? memories.filter(m => m.scope_kind === 'agent' && !known.has(m.scope_id || '')) : [];
  // Only an agent with a memory is on offer: one without would be an empty view.
  const withMemory = (agents || []).filter(a => memories.some(m => m.scope_kind === 'agent' && m.scope_id === a.id));
  // A scope that emptied (its last row deleted, its agent gone) falls back to global.
  const listed = scope === DELETED ? orphans.length > 0 : withMemory.some(a => a.id === scope);
  const view = scope && !listed ? '' : scope;
  const rows = view === '' ? memories.filter(m => m.scope_kind === 'global')
    : view === DELETED ? orphans
    : memories.filter(m => m.scope_kind === 'agent' && m.scope_id === view);

  const toForm = (m: Memory): MemoryFormData => ({ scope_kind: m.scope_kind, scope_id: m.scope_id || '', key: m.key, content: m.content, metadata: m.metadata || '' });
  // A new memory lands in the scope on view.
  const fresh: MemoryFormData = view && view !== DELETED
    ? { scope_kind: 'agent', scope_id: view, key: '', content: '', metadata: '' }
    : { scope_kind: 'global', scope_id: '', key: '', content: '', metadata: '' };

  // A saved memory is shown where it landed, whichever scope was on view.
  const saveAndShow = async (f: MemoryFormData) => {
    if (await save(f)) setScope(f.scope_kind === 'agent' ? f.scope_id : '');
  };

  const form = adding ? <MemoryForm key="add" initial={fresh} saving={saving} onSave={saveAndShow} onCancel={cancel} agents={agents} />
    : editing ? <MemoryForm key={editing.id} locked initial={toForm(editing)} saving={saving} onSave={saveAndShow} onCancel={cancel} onDelete={async () => { if (await remove(editing.id, editing.key)) cancel(); }} agents={agents} />
    : null;

  const filter = (
    <AgentPicker size="small" ariaLabel="Scope" agents={withMemory} value={view} onChange={setScope} emptyLabel="Global"
      extra={orphans.length ? { value: DELETED, label: `Deleted agents (${orphans.length})` } : undefined} />
  );

  return (
    <CrudPanel title="Memory" filter={filter} onAdd={startAdd} onCancel={cancel} form={form} loading={loading} isEmpty={rows.length === 0}
      empty="No global memories yet."
      emptyHint="A memory is text an agent reads with every request. What the model writes for itself during a session is in its Context panel.">
      {rows.map(m => (
        <ResourceRow key={m.id}
          title={m.key}
          sub={m.content.substring(0, 120) + (m.content.length > 120 ? '…' : '')}
          actions={view === DELETED
            ? <RowActionsMenu name={m.key} onDelete={() => void remove(m.id, m.key)} />
            : <RowActionsMenu name={m.key} onEdit={() => startEdit(m)} />}
        />
      ))}
    </CrudPanel>
  );
}

export default MemoryPanel;
