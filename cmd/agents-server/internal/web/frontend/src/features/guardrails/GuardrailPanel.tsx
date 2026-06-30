import { useState, type ChangeEvent } from 'react';
import { Button, TextInput, Label, SegmentedControl, Stack, PageHeader } from '@primer/react';
import { Blankslate } from '@primer/react/experimental';
import { api } from '@/lib/api';
import { useCrud } from '@/lib/hooks';
import { fc } from '@/lib/form';

interface Guardrail {
  id: string;
  name: string;
  description: string;
  type: string;
  mode: string;
  config: string;
  created_at: string;
  updated_at: string;
}

interface GuardrailFormData {
  name: string;
  description: string;
  type: string;
  mode: string;
  config?: string;
}

interface GuardrailFormProps {
  initial?: Guardrail | null;
  onSave: (form: GuardrailFormData) => void;
  onCancel?: (() => void) | null;
  onDelete?: (() => void) | null;
}

const TYPES = ['input', 'output'] as const;
const MODES = ['regex', 'max_length'] as const;
const MODE_LABELS: Record<string, string> = { regex: 'Regex Pattern', max_length: 'Max Length' };

function GuardrailForm({ initial, onSave, onCancel, onDelete }: GuardrailFormProps) {
  const [form, setForm] = useState<GuardrailFormData>(initial || {
    name: '', description: '', type: 'input', mode: 'regex',
  });
  const [pattern, setPattern] = useState<string>(() => {
    try { const c = JSON.parse((initial && initial.config) || '{}'); return c.pattern || ''; } catch { return ''; }
  });
  const [maxLength, setMaxLength] = useState<string | number>(() => {
    try { const c = JSON.parse((initial && initial.config) || '{}'); return c.max_length || 0; } catch { return 0; }
  });
  const set = (k: keyof GuardrailFormData, v: string) => setForm(prev => ({ ...prev, [k]: v }));

  const handleSave = () => {
    const config = form.mode === 'regex'
      ? { pattern }
      : { max_length: parseInt(String(maxLength)) || 0 };
    onSave({ ...form, config: JSON.stringify(config) });
  };

  return (
    <Stack gap="normal">
      {fc('Name',
        <TextInput
          value={form.name}
          onChange={(e: ChangeEvent<HTMLInputElement>) => set('name', e.target.value)}
          placeholder="e.g. block_profanity"
        />,
      )}
      {fc('Description',
        <TextInput block
          value={form.description || ''}
          onChange={(e: ChangeEvent<HTMLInputElement>) => set('description', e.target.value)}
          placeholder="What this guardrail does"
        />,
      )}
      {fc('Type',
        <SegmentedControl aria-label="Guardrail type" size="small">
          {TYPES.map(v => (
            <SegmentedControl.Button
              key={v}
              selected={form.type === v}
              onClick={() => set('type', v)}
            >
              {v.charAt(0).toUpperCase() + v.slice(1)}
            </SegmentedControl.Button>
          ))}
        </SegmentedControl>,
        'Applied on user input or model output',
      )}
      {fc('Mode',
        <SegmentedControl aria-label="Guardrail mode" size="small">
          {MODES.map(v => (
            <SegmentedControl.Button
              key={v}
              selected={form.mode === v}
              onClick={() => set('mode', v)}
            >
              {MODE_LABELS[v]}
            </SegmentedControl.Button>
          ))}
        </SegmentedControl>,
      )}
      {form.mode === 'regex' && fc('Pattern',
        <TextInput block
          value={pattern}
          onChange={(e: ChangeEvent<HTMLInputElement>) => setPattern(e.target.value)}
          placeholder="(?i)\bbadword\b"
          style={{ fontFamily: 'var(--fontStack-monospace)' }}
        />,
        'Go regexp syntax. Triggers when matched.',
      )}
      {form.mode === 'max_length' && fc('Max length',
        <TextInput
          type="number"
          min={1}
          value={maxLength || ''}
          onChange={(e: ChangeEvent<HTMLInputElement>) => setMaxLength(e.target.value)}
          placeholder="4096"
          style={{ width: '120px' }}
        />,
        'Maximum character count',
      )}
      <div className="form-actions">
        <Button onClick={handleSave} variant="primary">Save</Button>
        {onCancel && <Button onClick={onCancel}>Cancel</Button>}
        {onDelete && <Button onClick={onDelete} variant="danger" style={{ marginLeft: 'auto' }}>Delete</Button>}
      </div>
    </Stack>
  );
}

export function GuardrailPanel() {
  const { items: guardrails, adding, editing, startAdd, startEdit, cancel, save, remove } =
    useCrud<Guardrail, GuardrailFormData>(api.guardrails);

  const isBuiltin = (g: Guardrail): boolean => !g.id;

  return (
    <Stack gap="normal">
      <PageHeader>
        <PageHeader.TitleArea>
          <PageHeader.Title>Guardrails</PageHeader.Title>
        </PageHeader.TitleArea>
        {!adding && !editing && <PageHeader.Actions><Button onClick={startAdd} variant="primary" size="small">+ Add</Button></PageHeader.Actions>}
      </PageHeader>

      {adding && <GuardrailForm onSave={save} onCancel={cancel} />}
      {editing && <GuardrailForm initial={editing} onSave={save} onCancel={cancel} onDelete={() => { remove(editing.id); cancel(); }} />}

      {!adding && !editing && <div className="Box">
        {guardrails.map((g, i) => (
          <div key={g.id || ('builtin-' + i)} className="Box-row">
            <div className="resource-row-main">
              <div className="resource-row-title">
                {g.name}
                {isBuiltin(g) && <Label>built-in</Label>}
              </div>
              <div className="resource-row-meta">
                {[g.type, g.mode].filter(Boolean).join(' · ')}
                {g.description && (' — ' + g.description)}
              </div>
            </div>
            {!isBuiltin(g) && (
              <div className="resource-row-actions">
                <Button onClick={() => startEdit(g)} size="small" variant="invisible">Edit</Button>
              </div>
            )}
          </div>
        ))}
        {guardrails.length === 0 && (
          <Blankslate>
            <Blankslate.Description>
              No guardrails configured. Built-in guardrails (content_filter, max_input_length, max_output_length) are always available.
            </Blankslate.Description>
          </Blankslate>
        )}
      </div>}
    </Stack>
  );
}

export default GuardrailPanel;
