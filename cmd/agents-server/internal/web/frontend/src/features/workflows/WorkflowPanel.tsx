import { useState } from 'react';
import { Button, TextInput, Textarea, Label, CounterLabel, Select, IconButton, Stack, PageHeader } from '@primer/react';
import { Blankslate } from '@primer/react/experimental';
import { ChevronUpIcon, ChevronDownIcon, TrashIcon } from '@primer/octicons-react';
import { api } from '@/lib/api';
import { useApi, useCrud } from '@/lib/hooks';
import { fc } from '@/lib/form';
import { BADGE } from '@/lib/badges';
import '@/features/chat/workflow.css';

// One step: the agent that runs it and the prompt that starts its turn. The id
// is server-assigned and STABLE — inserting a step above another must not
// renumber what a run in flight or a retry is naming.
interface WorkflowStep {
  id?: string;
  name?: string;
  agent_config_id: string;
  prompt: string;
  compact_before?: boolean;
  // Where the sequence goes after this step. Empty on_success falls through to
  // the next step in the list; empty on_failure fails the workflow. "end" stops
  // there. Pointing BACKWARDS is how a sequence loops.
  on_success?: string;
  on_failure?: string;
}

interface Workflow {
  id: string;
  name: string;
  description?: string;
  steps: WorkflowStep[];
}

interface AgentRef {
  id: string;
  name: string;
}

interface WorkflowFormData {
  name: string;
  description: string;
  steps: WorkflowStep[];
}

// A step carries an id from the moment it is added: the edge pickers name steps
// by id, and a step with none cannot be pointed at until after a save.
const newStepId = () =>
  (globalThis.crypto?.randomUUID?.() ?? Math.random().toString(36).slice(2) + Date.now().toString(36));

const emptyStep = (): WorkflowStep => ({ id: newStepId(), name: '', agent_config_id: '', prompt: '' });

const stepLabel = (s: WorkflowStep, i: number) => (s.name || '').trim() || `Step ${i + 1}`;

// END is the reserved edge target that stops the execution (mirrors
// store.WorkflowStepEnd).
const END = 'end';

interface WorkflowFormProps {
  initial?: WorkflowFormData | null;
  onSave: (form: WorkflowFormData) => void;
  onCancel?: (() => void) | null;
  onDelete?: (() => void) | null;
  agents: AgentRef[] | null;
}

function WorkflowForm({ initial, onSave, onCancel, onDelete, agents }: WorkflowFormProps) {
  const [form, setForm] = useState<WorkflowFormData>(
    initial || { name: '', description: '', steps: [emptyStep()] },
  );

  const setStep = (i: number, patch: Partial<WorkflowStep>) =>
    setForm(prev => ({ ...prev, steps: prev.steps.map((s, j) => (j === i ? { ...s, ...patch } : s)) }));

  // Reordering is up/down rather than drag: the order is the whole meaning of a
  // workflow, and two buttons are exact, keyboard-reachable and need no
  // dependency. A step keeps its id as it moves, so a run in flight is unaffected.
  const move = (i: number, delta: number) =>
    setForm(prev => {
      const j = i + delta;
      if (j < 0 || j >= prev.steps.length) return prev;
      const steps = [...prev.steps];
      [steps[i], steps[j]] = [steps[j], steps[i]];
      return { ...prev, steps };
    });

  // Removing a step also resets edges that pointed at it (back to '' =
  // default) — the pickers no longer show the id, but a kept value would still
  // be saved, and the server rejects a dangling target.
  const removeStep = (i: number) =>
    setForm(prev => {
      const gone = prev.steps[i]?.id;
      const steps = prev.steps.filter((_, j) => j !== i).map(s =>
        gone && (s.on_success === gone || s.on_failure === gone)
          ? {
              ...s,
              on_success: s.on_success === gone ? '' : s.on_success,
              on_failure: s.on_failure === gone ? '' : s.on_failure,
            }
          : s);
      return { ...prev, steps };
    });

  return (
    <Stack gap="normal">
      {fc('Name', <TextInput block value={form.name}
        onChange={e => setForm(prev => ({ ...prev, name: e.target.value }))} placeholder="e.g. codegen" />,
        'How the agent refers to it')}

      {fc('Description', <TextInput block value={form.description}
        onChange={e => setForm(prev => ({ ...prev, description: e.target.value }))}
        placeholder="When to run this — e.g. Implement a feature end to end, with tests" />,
        'Required: the agent starts a workflow by matching a request against this, and that is the only way one starts')}


      <div className="form-group">
        <div className="form-group-title">Steps</div>
        {form.steps.map((step, i) => (
          <div key={step.id || i} className="wf-step">
            <div className="wf-step-head">
              {/* An ordinal, not a category — the counting pill, not a Label. */}
              <CounterLabel>{i + 1}</CounterLabel>
              <span className="wf-step-name">
                <TextInput block size="small"
                  value={step.name || ''}
                  onChange={e => setStep(i, { name: e.target.value })}
                  placeholder="step name (optional)"
                />
              </span>
              <IconButton icon={ChevronUpIcon} aria-label="Move up" size="small" variant="invisible"
                disabled={i === 0} onClick={() => move(i, -1)} />
              <IconButton icon={ChevronDownIcon} aria-label="Move down" size="small" variant="invisible"
                disabled={i === form.steps.length - 1} onClick={() => move(i, 1)} />
              <IconButton icon={TrashIcon} aria-label="Remove step" size="small" variant="invisible"
                disabled={form.steps.length === 1} onClick={() => removeStep(i)} />
            </div>
            <Select value={step.agent_config_id} onChange={e => setStep(i, { agent_config_id: e.target.value })} block>
              <Select.Option value="">Select an agent…</Select.Option>
              {(agents || []).map(a => <Select.Option key={a.id} value={a.id}>{a.name}</Select.Option>)}
            </Select>
            <Textarea block rows={2} value={step.prompt}
              onChange={e => setStep(i, { prompt: e.target.value })}
              placeholder="What this step should do — the previous steps are already in the conversation" />
            <label className="wf-step-opt">
              <input type="checkbox" checked={!!step.compact_before}
                onChange={e => setStep(i, { compact_before: e.target.checked })} />
              {' '}Compact the conversation before this step
            </label>
            {/* The only branching a workflow has. Both default to what a plain
                list already does, so a linear workflow never touches them. */}
            <div className="wf-step-edges">
              <label className="wf-step-opt">
                On success →{' '}
                <Select size="small" value={step.on_success || ''}
                  onChange={e => setStep(i, { on_success: e.target.value })}>
                  <Select.Option value="">{i === form.steps.length - 1 ? 'Finish' : 'Next step'}</Select.Option>
                  <Select.Option value={END}>Finish here</Select.Option>
                  {form.steps.map((o, j) => (j === i || !o.id ? null :
                    <Select.Option key={o.id} value={o.id}>{stepLabel(o, j)}</Select.Option>))}
                </Select>
              </label>
              <label className="wf-step-opt">
                On failure →{' '}
                <Select size="small" value={step.on_failure || ''}
                  onChange={e => setStep(i, { on_failure: e.target.value })}>
                  <Select.Option value="">Fail the workflow</Select.Option>
                  {form.steps.map((o, j) => (j === i || !o.id ? null :
                    <Select.Option key={o.id} value={o.id}>{stepLabel(o, j)}</Select.Option>))}
                </Select>
              </label>
            </div>
          </div>
        ))}
        <Button size="small" onClick={() => setForm(prev => ({ ...prev, steps: [...prev.steps, emptyStep()] }))}>
          + Add step
        </Button>
      </div>

      <div className="form-actions">
        <Button onClick={() => onSave(form)} variant="primary">Save</Button>
        {onCancel && <Button onClick={onCancel}>Cancel</Button>}
        {onDelete && <Button onClick={onDelete} variant="danger" style={{ marginLeft: 'auto' }}>Delete</Button>}
      </div>
    </Stack>
  );
}

export function WorkflowPanel() {
  const { items: workflows, adding, editing, startAdd, startEdit, cancel, save, remove } =
    useCrud<Workflow, WorkflowFormData>(api.workflows);
  const { data: agents } = useApi<AgentRef[]>(() => api.agents.list() as Promise<AgentRef[]>);

  const agentName = (id: string) => (agents || []).find(a => a.id === id)?.name || id.slice(0, 8);

  return (
    <Stack gap="normal">
      <PageHeader>
        <PageHeader.TitleArea>
          <PageHeader.Title>Workflows</PageHeader.Title>
        </PageHeader.TitleArea>
        {!adding && !editing && <PageHeader.Actions><Button onClick={startAdd} variant="primary" size="small">+ Add</Button></PageHeader.Actions>}
        <PageHeader.Description>
          A fixed sequence of steps, each an ordinary turn by the agent you pick. It runs in the
          background, in a session of its own, and the agent starts one when a request matches its
          description.
        </PageHeader.Description>
      </PageHeader>

      {adding && <WorkflowForm onSave={save} onCancel={cancel} agents={agents} />}
      {editing && (
        <WorkflowForm
          initial={{
            name: editing.name,
            description: editing.description || '',
            steps: editing.steps || [],
          }}
          onSave={save}
          onCancel={cancel}
          onDelete={() => { remove(editing.id); cancel(); }}
          agents={agents}
        />
      )}

      {!adding && !editing && <div className="Box">
        {workflows.map(w => (
          <div key={w.id} className="Box-row">
            <div className="resource-row-main">
              <div className="resource-row-head">
                <span className="resource-row-title">{w.name}</span>
                <Label variant={BADGE.count}>{'Steps·' + (w.steps || []).length}</Label>
              </div>
              <div className="resource-row-sub">
                {(w.steps || []).map(s => s.name || agentName(s.agent_config_id)).join(' → ')}
              </div>
            </div>
            <div className="resource-row-actions">
              <Button onClick={() => startEdit(w)} size="small" variant="invisible">Edit</Button>
            </div>
          </div>
        ))}
        {workflows.length === 0 && (
          <Blankslate>
            <Blankslate.Description>
              No workflows yet. A workflow runs a fixed sequence of agents on one session — plan, then execute,
              then verify, each on the model you choose for it.
            </Blankslate.Description>
          </Blankslate>
        )}
      </div>}
    </Stack>
  );
}

export default WorkflowPanel;
