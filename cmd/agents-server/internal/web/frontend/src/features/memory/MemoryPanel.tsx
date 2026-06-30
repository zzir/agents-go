import { useState } from 'react';
import { Button, TextInput, Textarea, Label, Select, Stack, PageHeader } from '@primer/react';
import { Blankslate } from '@primer/react/experimental';
import { api } from '@/lib/api';
import { useApi, useCrud } from '@/lib/hooks';
import { fc } from '@/lib/form';
import { JsonField } from '@/lib/JsonField';

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
  agents: AgentConfig[] | null;
}

function MemoryForm({ initial, onSave, onCancel, onDelete, agents }: MemoryFormProps) {
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
      <div className="form-actions">
        <Button onClick={() => onSave(form)} variant="primary">
          Save
        </Button>
        {onCancel && <Button onClick={onCancel}>Cancel</Button>}
        {onDelete && <Button onClick={onDelete} variant="danger" style={{ marginLeft: 'auto' }}>Delete</Button>}
      </div>
    </Stack>
  );
}

export function MemoryPanel() {
  const { items: memories, adding, editing, startAdd, startEdit, cancel, save, remove } =
    useCrud<Memory, MemoryFormData>(api.memories);
  const { data: agents } = useApi<AgentConfig[]>(
    () => api.agents.list() as Promise<AgentConfig[]>,
  );

  const agentName = (id: string) => {
    if (!id || !agents) return 'Global';
    const found = agents.find(a => a.id === id);
    return found ? found.name : id.substring(0, 8);
  };

  return (
    <Stack gap="normal">
      <PageHeader>
        <PageHeader.TitleArea>
          <PageHeader.Title>Memory</PageHeader.Title>
        </PageHeader.TitleArea>
        {!adding && !editing && <PageHeader.Actions><Button onClick={startAdd} variant="primary" size="small">+ Add</Button></PageHeader.Actions>}
      </PageHeader>

      {adding && (
        <MemoryForm onSave={save} onCancel={cancel} agents={agents} />
      )}
      {editing && (
        <MemoryForm initial={editing} onSave={save} onCancel={cancel} onDelete={() => { remove(editing.id); cancel(); }} agents={agents} />
      )}

      {!adding && !editing && <div className="Box">
        {memories.map(m => (
          <div key={m.id} className="Box-row">
            <div className="resource-row-main">
              <div className="resource-row-meta">
                <Label variant={m.agent_config_id ? 'accent' : 'secondary'}>
                  {agentName(m.agent_config_id)}
                </Label>
                <span className="resource-row-title">{m.key}</span>
              </div>
              <div className="resource-row-sub">
                {m.content.substring(0, 120) +
                  (m.content.length > 120 ? '...' : '')}
              </div>
            </div>
            <div className="resource-row-actions">
              <Button onClick={() => startEdit(m)} size="small" variant="invisible">
                Edit
              </Button>
            </div>
          </div>
        ))}
        {memories.length === 0 && (
          <Blankslate>
            <Blankslate.Description>No memories stored.</Blankslate.Description>
          </Blankslate>
        )}
      </div>}
    </Stack>
  );
}

export default MemoryPanel;
