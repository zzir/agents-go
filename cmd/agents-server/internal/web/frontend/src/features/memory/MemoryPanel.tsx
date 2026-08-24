import { useState } from 'react';
import { TextInput, Textarea, Label, Select, Stack } from '@primer/react';
import { FormActions } from '@/components/FormActions';
import { CrudPanel, RowActionsMenu } from '@/components/CrudPanel';
import { ResourceRow } from '@/components/ResourceRow';
import { api } from '@/lib/api';
import { nameOf } from '@/lib/named';
import { useApi, useCrud } from '@/lib/hooks';
import { fc } from '@/lib/form';
import { JsonField } from '@/lib/JsonField';
import { BADGE } from '@/lib/badges';

interface Memory {
  id: string;
  agent_config_id: string;
  key: string;
  content: string;
  metadata: string;
  created_at: string;
  updated_at: string;
}

interface AgentConfig {
  id: string;
  name: string;
}

interface MemoryFormData {
  key: string;
  content: string;
  metadata: string;
  agent_config_id: string;
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
    initial || { key: '', content: '', metadata: '', agent_config_id: '' },
  );
  const set = (k: keyof MemoryFormData, v: string) =>
    setForm(prev => ({ ...prev, [k]: v }));

  return (
    <Stack gap="normal">
      {fc(
        'Agent',
        <Select
          value={form.agent_config_id || ''}
          onChange={e => set('agent_config_id', e.target.value)}
        >
          <Select.Option value="">(Global - all agents)</Select.Option>
          {agents &&
            agents.map(a => (
              <Select.Option key={a.id} value={a.id}>
                {a.name}
              </Select.Option>
            ))}
        </Select>,
      )}
      {fc(
        'Key',
        <TextInput block
          value={form.key}
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
  const { items: memories, adding, editing, startAdd, startEdit, cancel, save, saving, remove } =
    useCrud<Memory, MemoryFormData>(api.memories);
  const { data: agents } = useApi<AgentConfig[]>(
    () => api.agents.list() as Promise<AgentConfig[]>,
  );

  const agentName = (id: string) => (!id || !agents ? 'Global' : nameOf(agents, id));

  const form = adding ? <MemoryForm saving={saving} onSave={save} onCancel={cancel} agents={agents} />
    : editing ? <MemoryForm saving={saving} initial={editing} onSave={save} onCancel={cancel} onDelete={async () => { if (await remove(editing.id, editing.key)) cancel(); }} agents={agents} />
    : null;

  return (
    <CrudPanel title="Memory" onAdd={startAdd} onCancel={cancel} form={form} isEmpty={memories.length === 0} empty="No memories stored.">
      {/* Global is the default and says nothing — only a SCOPED memory
          carries a badge: the agent it belongs to. */}
      {memories.map(m => (
        <ResourceRow key={m.id}
          title={m.key}
          badges={m.agent_config_id && <Label variant={BADGE.ref}>{agentName(m.agent_config_id)}</Label>}
          sub={m.content.substring(0, 120) + (m.content.length > 120 ? '...' : '')}
          actions={<RowActionsMenu name={m.key} onEdit={() => startEdit(m)} />}
        />
      ))}
    </CrudPanel>
  );
}

export default MemoryPanel;
