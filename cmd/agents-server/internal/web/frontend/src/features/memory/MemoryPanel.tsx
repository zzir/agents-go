import { useState } from 'react';
import { TextInput, Textarea, Label, Stack, Select } from '@primer/react';
import { AgentAvatar } from '@/components/AgentAvatar';
import { AgentPicker } from '@/components/AgentPicker';
import { FormActions } from '@/components/FormActions';
import { CrudPanel, RowActionsMenu } from '@/components/CrudPanel';
import { ResourceRow } from '@/components/ResourceRow';
import { api } from '@/lib/api';
import { nameOf } from '@/lib/named';
import { useApi, useCrud } from '@/lib/hooks';
import { fc } from '@/lib/form';
import { JsonField } from '@/lib/JsonField';
import { BADGE } from '@/lib/badges';

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
  initial?: MemoryFormData | null;
  onSave: (form: MemoryFormData) => void;
  onCancel?: (() => void) | null;
  onDelete?: (() => void) | null;
  saving?: boolean;
  agents: AgentConfig[] | null;
}

function MemoryForm({ initial, onSave, onCancel, onDelete, saving, agents }: MemoryFormProps) {
  const [form, setForm] = useState<MemoryFormData>(
    initial || { scope_kind: 'global', scope_id: '', key: '', content: '', metadata: '' },
  );
  const set = (k: keyof MemoryFormData, v: string) =>
    setForm(prev => ({ ...prev, [k]: v }));
  // Scope and key identify a memory; an edit keeps them.
  const locked = !!initial;

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

export function MemoryPanel() {
  const { items: memories, loading, adding, editing, startAdd, startEdit, cancel, save, saving, remove } =
    useCrud<Memory, MemoryFormData>(api.memories, 'memories');
  const { data: agents } = useApi<AgentConfig[]>(() => api.agents.list() as Promise<AgentConfig[]>, [], 'agents');

  const agentName = (id: string) => (!id || !agents ? 'Global' : nameOf(agents, id));
  const toForm = (m: Memory): MemoryFormData => ({ scope_kind: m.scope_kind, scope_id: m.scope_id || '', key: m.key, content: m.content, metadata: m.metadata || '' });

  const form = adding ? <MemoryForm saving={saving} onSave={save} onCancel={cancel} agents={agents} />
    : editing ? <MemoryForm saving={saving} initial={toForm(editing)} onSave={save} onCancel={cancel} onDelete={async () => { if (await remove(editing.id, editing.key)) cancel(); }} agents={agents} />
    : null;

  return (
    <CrudPanel title="Memory" onAdd={startAdd} onCancel={cancel} form={form} loading={loading} isEmpty={memories.length === 0}
      empty="No memories yet." emptyHint="A memory is text an agent reads with every request. What the model writes for itself during a conversation is in that session's Context panel.">
      {/* Global is the default and says nothing — only a SCOPED memory
          carries a badge: the agent it belongs to. A model-written one says so. */}
      {memories.map(m => (
        <ResourceRow key={m.id}
          title={m.key}
          badges={<>
            {m.scope_kind === 'agent' && m.scope_id && <Label variant={BADGE.ref}>
              <span className="agent-inline">
                <AgentAvatar name={agentName(m.scope_id)} avatar={(agents || []).find(a => a.id === m.scope_id)?.avatar} size={16} />
                {agentName(m.scope_id)}
              </span>
            </Label>}
            {m.written_by === 'model' && <Label variant="attention">model</Label>}
          </>}
          sub={m.content.substring(0, 120) + (m.content.length > 120 ? '…' : '')}
          actions={<RowActionsMenu name={m.key} onEdit={() => startEdit(m)} />}
        />
      ))}
    </CrudPanel>
  );
}

export default MemoryPanel;
