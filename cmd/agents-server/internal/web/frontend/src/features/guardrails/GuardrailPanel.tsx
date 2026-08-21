import { useState, type ChangeEvent } from 'react';
import { Button, TextInput, Label, SegmentedControl, Stack, PageHeader, Checkbox, FormControl } from '@primer/react';
import { FormActions } from '@/components/FormActions';
import { Blankslate } from '@primer/react/experimental';
import { api } from '@/lib/api';
import { useCrud } from '@/lib/hooks';
import { fc } from '@/lib/form';
import { BADGE } from '@/lib/badges';

// config travels as a JSON object in both directions (the API-wide contract
// for config blobs) — never as a stringified JSON payload.
interface GuardrailConfig {
  pattern?: string;
  max_length?: number;
}

interface Guardrail {
  id: string;
  name: string;
  description: string;
  // stages are the run stages this guardrail inspects. One definition covering
  // several is the SDK's model — a content scanner that should see the input,
  // the tool arguments and the final output is one guardrail, not three.
  stages: string[];
  mode: string;
  config?: GuardrailConfig;
  blocking?: boolean;
}

interface GuardrailFormData {
  name: string;
  description: string;
  stages: string[];
  mode: string;
  config?: GuardrailConfig;
  blocking?: boolean;
}

interface GuardrailFormProps {
  initial?: Guardrail | null;
  onSave: (form: GuardrailFormData) => void;
  onCancel?: (() => void) | null;
  onDelete?: (() => void) | null;
}

// Mirrors agents/guardrail.go — keep in sync.
const STAGES = ['input', 'output', 'tool_input', 'tool_output'] as const;
const STAGE_LABELS: Record<string, string> = {
  input: 'Run input',
  output: 'Final output',
  tool_input: 'Tool arguments',
  tool_output: 'Tool result',
};
const MODES = ['regex', 'max_length'] as const;
const MODE_LABELS: Record<string, string> = { regex: 'Regex Pattern', max_length: 'Max Length' };

function GuardrailForm({ initial, onSave, onCancel, onDelete }: GuardrailFormProps) {
  const [form, setForm] = useState<GuardrailFormData>(initial || {
    name: '', description: '', stages: ['input'], mode: 'regex', blocking: false,
  });
  const [pattern, setPattern] = useState<string>(initial?.config?.pattern || '');
  const [maxLength, setMaxLength] = useState<string | number>(initial?.config?.max_length || 0);
  const set = (k: keyof GuardrailFormData, v: string | boolean | string[]) => setForm(prev => ({ ...prev, [k]: v }));
  const stages = form.stages || [];
  const toggleStage = (st: string) => set('stages', stages.includes(st) ? stages.filter(s => s !== st) : [...stages, st]);

  const handleSave = () => {
    const config: GuardrailConfig = form.mode === 'regex'
      ? { pattern }
      : { max_length: parseInt(String(maxLength)) || 0 };
    onSave({ ...form, config });
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
      {fc('Stages',
        <Stack gap="condensed">
          {STAGES.map(v => (
            <FormControl key={v}>
              <Checkbox checked={stages.includes(v)} onChange={() => toggleStage(v)} />
              <FormControl.Label>{STAGE_LABELS[v]}</FormControl.Label>
            </FormControl>
          ))}
        </Stack>,
        'Where this guardrail runs. One guardrail can cover several stages — a content scanner that should see the input, the tool arguments and the final output is one definition, not three.',
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
          block
          type="number"
          min={1}
          value={maxLength || ''}
          onChange={(e: ChangeEvent<HTMLInputElement>) => setMaxLength(e.target.value)}
          placeholder="4096"
        />,
        'Maximum character count',
      )}
      {stages.includes('input') && (
        <FormControl>
          <Checkbox checked={!!form.blocking} onChange={(e: ChangeEvent<HTMLInputElement>) => set('blocking', e.target.checked)} />
          <FormControl.Label>Blocking</FormControl.Label>
          <FormControl.Caption>At the input stage, run before the model call (a gate) instead of racing it — a tripwire then prevents the call and any token spend</FormControl.Caption>
        </FormControl>
      )}
      <FormActions onSave={handleSave} onCancel={onCancel} onDelete={onDelete} />
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
      {editing && <GuardrailForm initial={editing} onSave={save} onCancel={cancel} onDelete={async () => { if (await remove(editing.id, editing.name)) cancel(); }} />}

      {!adding && !editing && <div className="Box">
        {guardrails.map((g, i) => (
          <div key={g.id || ('builtin-' + i)} className="Box-row">
            <div className="resource-row-main">
              <div className="resource-row-head">
                <span className="resource-row-title">{g.name}</span>
                {isBuiltin(g) && <Label variant={BADGE.builtin}>built-in</Label>}
              </div>
              <div className="resource-row-sub">
                {[(g.stages || []).map(st => STAGE_LABELS[st] || st).join(', '), g.mode].filter(Boolean).join(' · ')}
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
