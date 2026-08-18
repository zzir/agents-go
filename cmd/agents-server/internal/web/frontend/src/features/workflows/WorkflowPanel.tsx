import { useState } from 'react';
import { Button, TextInput, Textarea, Label, CounterLabel, Select, IconButton, Stack, Dialog } from '@primer/react';
import { Blankslate, Table } from '@primer/react/experimental';
import { ChevronUpIcon, ChevronDownIcon, TrashIcon, PlayIcon, ZapIcon } from '@primer/octicons-react';
import { api } from '@/lib/api';
import { PAGE_SIZE, useApi, useCrud, usePage } from '@/lib/hooks';
import { fc } from '@/lib/form';
import { BADGE } from '@/lib/badges';
import { toast } from '@/lib/toast';
import { useMermaidSvg } from '@/features/chat/MermaidBlock';
import { TriggersDialog } from '@/features/workflows/TriggersDialog';
import { SessionPicker } from '@/features/sessions/SessionPicker';
import { bindingWorkDirIssue, type SandboxConfigLite } from '@/lib/binding';
import '@/features/chat/workflow.css';
import './workflow-panel.css';
import './hub.css';

// One step: the agent that runs it and the prompt that starts its turn. The id
// is server-assigned and STABLE — inserting a step above another must not
// renumber what a run in flight or a retry is naming.
interface WorkflowStep {
  id?: string;
  name?: string;
  agent_config_id: string;
  prompt: string;
  compact_before?: boolean;
  // Hold the sequence here until a person approves it from the chat.
  pause_before?: boolean;
  // A CHECK: the last line of the step's output (PASS/FAIL, or the gate's own
  // words) picks the edge instead of the run's outcome. Absent = plain step.
  gate?: { pass?: string; fail?: string } | null;
  // Where the sequence goes after this step. Empty on_success falls through to
  // the next step in the list; empty on_failure fails the workflow. "end" stops
  // there. Pointing BACKWARDS is how a sequence loops.
  on_success?: string;
  on_failure?: string;
}

// What one execution may spend before it is stopped, failed with the reason;
// 0/absent = no bound. Steps and minutes count step runs (a pause on a person
// costs nothing), tokens every model call on the execution's session. Laps
// bound a LOOP: how many times one execution may take the same backward edge
// (verify → exec) — 0/absent is the server's default of 3, not no bound.
interface WorkflowBudget {
  max_steps?: number;
  max_tokens?: number;
  max_minutes?: number;
  max_laps?: number;
}

interface Workflow {
  id: string;
  name: string;
  description?: string;
  steps: WorkflowStep[];
  budget?: WorkflowBudget;
}

interface AgentRef {
  id: string;
  name: string;
}

interface WorkflowFormData {
  name: string;
  description: string;
  steps: WorkflowStep[];
  budget: WorkflowBudget;
}

// A step carries an id from the moment it is added: the edge pickers name steps
// by id, and a step with none cannot be pointed at until after a save.
const newStepId = () =>
  (globalThis.crypto?.randomUUID?.() ?? Math.random().toString(36).slice(2) + Date.now().toString(36));

const emptyStep = (over: Partial<WorkflowStep> = {}): WorkflowStep => ({ id: newStepId(), name: '', agent_config_id: '', prompt: '', ...over });

const stepLabel = (s: WorkflowStep, i: number) => (s.name || '').trim() || `Step ${i + 1}`;

// END is the reserved edge target that stops the execution (mirrors
// store.WorkflowStepEnd).
const END = 'end';

// Starting points for the two shapes a sequence usually takes. Agents are left
// for the author to pick — a template cannot know which exist.
const TEMPLATES: { key: string; label: string; form: () => WorkflowFormData }[] = [
  {
    key: 'plan-exec-verify', label: 'plan → exec → verify',
    form: () => {
      const plan = emptyStep({ name: 'plan', prompt: 'Read the brief and the codebase. Write a short plan: the files to change and how, the tests to add. Do not change anything yet.' });
      const exec = emptyStep({ name: 'exec', prompt: 'Carry out the plan above. Make the changes and add the tests.' });
      const verify = emptyStep({ name: 'verify', prompt: 'Run the tests and review the diff against the plan. Report what passes and what does not.', gate: {}, on_success: END, on_failure: exec.id });
      return { name: 'build', description: 'Implement a feature end to end, with tests', steps: [plan, exec, verify], budget: {} };
    },
  },
  {
    key: 'review-fix', label: 'review → fix loop',
    form: () => {
      const review = emptyStep({ name: 'review', prompt: 'Review the changes in the working directory for correctness and style. List concrete problems, or say there are none.', gate: {} });
      const fix = emptyStep({ name: 'fix', prompt: 'Fix every problem the review listed.', on_success: review.id });
      review.on_success = END;
      review.on_failure = fix.id;
      return { name: 'review', description: 'Review the current changes and fix what the review finds', steps: [review, fix], budget: {} };
    },
  },
];

// promptRows sizes the prompt box to its text: a one-liner stays small, a
// paragraph gets room, nothing scrolls before a dozen lines.
const promptRows = (text: string) => Math.min(12, Math.max(3, text.split('\n').length + 1));

// edgeSummary is the sequence's shape in words, shown only when a step names
// an edge — a plain list needs no diagram, and a loop is hard to read off
// three dropdowns.
function edgeSummary(steps: WorkflowStep[]): string[] {
  const nameOf = (id?: string, i?: number) => {
    if (id === END) return 'end';
    if (!id) return i !== undefined && i + 1 < steps.length ? stepLabel(steps[i + 1], i + 1) : 'end';
    const j = steps.findIndex(s => s.id === id);
    return j >= 0 ? stepLabel(steps[j], j) : id;
  };
  if (!steps.some(s => s.on_success || s.on_failure || s.gate)) return [];
  return steps.map((s, i) => {
    const ok = `${s.gate ? 'PASS' : 'ok'} → ${nameOf(s.on_success, i)}`;
    const bad = `${s.gate ? 'FAIL' : 'error'} → ${s.on_failure ? nameOf(s.on_failure) : 'stop, failed'}`;
    return `${stepLabel(s, i)}: ${ok} · ${bad}`;
  });
}

// edgeGraph is the same shape as a flowchart: one node per step, a solid
// edge for the success side, a dotted one for the failure side, and two
// terminals. Empty when no step names an edge, like edgeSummary.
function edgeGraph(steps: WorkflowStep[]): string {
  if (!steps.some(s => s.on_success || s.on_failure || s.gate)) return '';
  const q = (t: string) => '"' + t.replace(/"/g, '#quot;') + '"';
  const node = (id?: string, i?: number): string => {
    if (id === END) return 'END';
    if (!id) return i !== undefined && i + 1 < steps.length ? `n${i + 1}` : 'END';
    const j = steps.findIndex(s => s.id === id);
    return j >= 0 ? `n${j}` : 'END';
  };
  const lines = ['flowchart LR'];
  steps.forEach((s, i) => lines.push(`  n${i}[${q(stepLabel(s, i))}]`));
  lines.push('  END((end))', '  FAILED((failed))');
  steps.forEach((s, i) => {
    lines.push(`  n${i} -->|${s.gate ? 'PASS' : 'ok'}| ${node(s.on_success, i)}`);
    lines.push(`  n${i} -.->|${s.gate ? 'FAIL' : 'error'}| ${s.on_failure ? node(s.on_failure) : 'FAILED'}`);
  });
  return lines.join('\n');
}

// EdgeGraph draws the sequence; until mermaid has rendered (or if it cannot)
// the same shape stands in words.
function EdgeGraph({ steps }: { steps: WorkflowStep[] }) {
  const { svg, failed } = useMermaidSvg(edgeGraph(steps));
  const summary = edgeSummary(steps);
  if (svg && !failed) {
    return <div className="wf-edge-graph" aria-label={summary.join('; ')} dangerouslySetInnerHTML={{ __html: svg }} />;
  }
  return (
    <div className="wf-edge-summary">
      {summary.map(line => <div key={line}>{line}</div>)}
    </div>
  );
}

interface WorkflowFormProps {
  initial?: WorkflowFormData | null;
  onSave: (form: WorkflowFormData) => void;
  onCancel?: (() => void) | null;
  onDelete?: (() => void) | null;
  agents: AgentRef[] | null;
}

function WorkflowForm({ initial, onSave, onCancel, onDelete, agents }: WorkflowFormProps) {
  const [form, setForm] = useState<WorkflowFormData>(
    initial || { name: '', description: '', steps: [emptyStep()], budget: {} },
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

  const branching = form.steps.some(s => s.on_success || s.on_failure || s.gate);

  return (
    <Stack gap="normal">
      {fc('Name', <TextInput block value={form.name}
        onChange={e => setForm(prev => ({ ...prev, name: e.target.value }))} placeholder="e.g. codegen" />,
        'How the agent refers to it')}

      {fc('Description', <TextInput block value={form.description}
        onChange={e => setForm(prev => ({ ...prev, description: e.target.value }))}
        placeholder="When to run this — e.g. Implement a feature end to end, with tests" />,
        'Required: the agent starts a workflow by matching a request against this')}


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
              {/* The agent beside the name, at a fixed width: the name takes the rest. */}
              <span className="wf-step-agent">
                <Select block size="small" aria-label="Agent" value={step.agent_config_id}
                  onChange={e => setStep(i, { agent_config_id: e.target.value })}>
                  <Select.Option value="">Select an agent…</Select.Option>
                  {(agents || []).map(a => <Select.Option key={a.id} value={a.id}>{a.name}</Select.Option>)}
                </Select>
              </span>
              <IconButton icon={ChevronUpIcon} aria-label="Move up" size="small" variant="invisible"
                disabled={i === 0} onClick={() => move(i, -1)} />
              <IconButton icon={ChevronDownIcon} aria-label="Move down" size="small" variant="invisible"
                disabled={i === form.steps.length - 1} onClick={() => move(i, 1)} />
              <IconButton icon={TrashIcon} aria-label="Remove step" size="small" variant="invisible"
                disabled={form.steps.length === 1} onClick={() => removeStep(i)} />
            </div>
            <Textarea block rows={promptRows(step.prompt)} value={step.prompt}
              onChange={e => setStep(i, { prompt: e.target.value })}
              placeholder="What this step should do — the previous steps are already in the conversation" />
            <div className="wf-step-opts">
              <label className="wf-step-opt">
                <input type="checkbox" checked={!!step.compact_before}
                  onChange={e => setStep(i, { compact_before: e.target.checked })} />
                {' '}Compact the conversation before this step
              </label>
              <label className="wf-step-opt">
                <input type="checkbox" checked={!!step.pause_before}
                  onChange={e => setStep(i, { pause_before: e.target.checked })} />
                {' '}Ask me before this step runs
              </label>
              <label className="wf-step-opt">
                <input type="checkbox" checked={!!step.gate}
                  onChange={e => setStep(i, { gate: e.target.checked ? {} : null })} />
                {' '}This step is a check: its last line, PASS or FAIL, picks the edge
              </label>
              {step.gate && (
                <span className="wf-step-gate">
                  <TextInput size="small" value={step.gate.pass || ''} placeholder="PASS"
                    aria-label="Pass word" onChange={e => setStep(i, { gate: { ...step.gate, pass: e.target.value } })} />
                  <TextInput size="small" value={step.gate.fail || ''} placeholder="FAIL"
                    aria-label="Fail word" onChange={e => setStep(i, { gate: { ...step.gate, fail: e.target.value } })} />
                </span>
              )}
            </div>
            {/* The only branching a workflow has. Both default to what a plain
                list already does, so a linear workflow never touches them. */}
            <div className="wf-step-edges">
              <label className="wf-step-opt">
                {step.gate ? 'On PASS →' : 'On success →'}{' '}
                <Select size="small" value={step.on_success || ''}
                  onChange={e => setStep(i, { on_success: e.target.value })}>
                  <Select.Option value="">{i === form.steps.length - 1 ? 'Finish' : 'Next step'}</Select.Option>
                  <Select.Option value={END}>Finish here</Select.Option>
                  {form.steps.map((o, j) => (j === i || !o.id ? null :
                    <Select.Option key={o.id} value={o.id}>{stepLabel(o, j)}</Select.Option>))}
                </Select>
              </label>
              <label className="wf-step-opt">
                {step.gate ? 'On FAIL →' : 'On failure →'}{' '}
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
        {branching && <EdgeGraph steps={form.steps} />}
      </div>

      {/* Bounds on ONE execution: checked before each step launches, so a run
          in flight is never cut. Blank = no bound. */}
      <div className="form-group">
        <div className="form-group-title">Budget per run</div>
        <div className="wf-budget">
          {([
            ['max_steps', 'steps', 'Step launches, retries included (at most 50)', '50'],
            ['max_tokens', 'tokens', 'Input + output of every model call', '∞'],
            ['max_minutes', 'minutes', 'Step run time; waiting for you costs nothing', '∞'],
            ['max_laps', 'laps', 'Times one run may take the same backward edge (verify → exec) — a loop that keeps returning is stopped', '3'],
          ] as const).map(([key, unit, hint, blank]) => (
            <label key={key} className="wf-step-opt" title={hint}>
              <TextInput size="small" type="number" min={0} value={form.budget[key] || ''}
                aria-label={'max ' + unit} placeholder={blank}
                onChange={e => setForm(prev => ({ ...prev, budget: { ...prev.budget, [key]: Math.max(0, Number(e.target.value) || 0) } }))} />
              {' '}{unit}
            </label>
          ))}
        </div>
        <div className="wf-run-hint">Over any of these the execution stops, failed with the reason. Laps default to 3: a loop that keeps returning to the same step is not converging.</div>
      </div>

      <div className="form-actions">
        <Button onClick={() => onSave(form)} variant="primary">Save</Button>
        {onCancel && <Button onClick={onCancel}>Cancel</Button>}
        {onDelete && <Button onClick={onDelete} variant="danger" style={{ marginLeft: 'auto' }}>Delete</Button>}
      </div>
    </Stack>
  );
}

// RunDialog asks for the brief and the conversation to report back to —
// the one the person came from by default — and starts the workflow: the
// person's own start, the same one the agent's tool makes. A conversation
// with no project bound is offered one here: the execution runs on the
// conversation's binding, and without one it has no file or command tools.
function RunDialog({ workflow, sessionId, onClose }: { workflow: Workflow; sessionId: string | null; onClose: () => void }) {
  const [target, setTarget] = useState(sessionId || '');
  const [input, setInput] = useState('');
  const [busy, setBusy] = useState(false);
  const { data: targetSession } = useApi<{ sandbox_id?: string } | null>(
    () => (target ? api.sessions.get(target) as Promise<{ sandbox_id?: string }> : Promise.resolve(null)), [target]);
  const { data: sandboxes } = useApi<SandboxConfigLite[]>(() => api.sandboxes.list() as Promise<SandboxConfigLite[]>);
  const [sandboxId, setSandboxId] = useState('');
  const [workDir, setWorkDir] = useState('');
  const unbound = !!target && !!targetSession && !targetSession.sandbox_id;
  const sandbox = (sandboxes || []).find(sb => sb.id === sandboxId);
  const editable = !!sandbox?.work_dir_editable;
  const workDirIssue = sandboxId ? bindingWorkDirIssue(sandbox, workDir) : null;
  const run = async () => {
    setBusy(true);
    try {
      const body: { session_id: string; input: string; sandbox_id?: string; work_dir?: string } = { session_id: target, input: input.trim() };
      if (unbound && sandboxId) {
        body.sandbox_id = sandboxId;
        if (editable && workDir.trim()) body.work_dir = workDir.trim();
      }
      await api.workflows.run(workflow.id, body);
      toast.success(`Started "${workflow.name}" in the background — the result comes back to the conversation`);
      onClose();
    } catch (e) {
      toast.error((e as Error).message || 'Could not start the workflow');
    } finally {
      setBusy(false);
    }
  };
  return (
    <Dialog title={`Run "${workflow.name}"`} onClose={onClose} width="large"
      footerButtons={[
        { buttonType: 'default', content: 'Cancel', onClick: onClose },
        { buttonType: 'primary', content: busy ? 'Starting…' : 'Run', onClick: run, disabled: busy || !target || !!workDirIssue },
      ]}>
      <Stack gap="condensed">
        <div className="wf-run-hint">
          It runs in a session of its own and cannot see the conversation: put everything it needs to know in the brief.
        </div>
        {fc('Conversation', <SessionPicker value={target} onChange={setTarget} />, 'Where the result comes back')}
        {unbound && (
          <>
            {fc('Project', <Select block value={sandboxId} onChange={e => { setSandboxId(e.target.value); setWorkDir((sandboxes || []).find(sb => sb.id === e.target.value)?.default_work_dir || ''); }}>
              <Select.Option value="">None — chat only, no file or command tools</Select.Option>
              {(sandboxes || []).map(sb => <Select.Option key={sb.id} value={sb.id}>{sb.name}{sb.type ? ` (${sb.type})` : ''}</Select.Option>)}
            </Select>, 'This conversation has no project bound yet; the one picked here becomes its binding, as a first message\'s would')}
            {sandboxId && editable && fc('Directory', <TextInput block value={workDir} onChange={e => setWorkDir(e.target.value)}
              placeholder={sandbox?.default_work_dir || ''} validationStatus={workDirIssue ? 'error' : undefined} />,
              workDirIssue || 'The project directory the steps work in')}
          </>
        )}
        <Textarea block rows={6} value={input} onChange={e => setInput(e.target.value)} autoFocus
          placeholder="What this run is about — the brief that leads the first step" />
      </Stack>
    </Dialog>
  );
}

export function WorkflowPanel({ sessionId }: { sessionId: string | null }) {
  const { items: workflows, adding, editing, startAdd, startEdit, cancel, save, remove } =
    useCrud<Workflow, WorkflowFormData>(api.workflows);
  const { data: agents } = useApi<AgentRef[]>(() => api.agents.list() as Promise<AgentRef[]>);
  // A template pre-fills the add form; cleared when the form closes.
  const [template, setTemplate] = useState<WorkflowFormData | null>(null);
  const [running, setRunning] = useState<Workflow | null>(null);
  const [triggersFor, setTriggersFor] = useState<Workflow | null>(null);

  const agentName = (id: string) => (agents || []).find(a => a.id === id)?.name || id.slice(0, 8);
  const closeForm = () => { setTemplate(null); cancel(); };
  const page = usePage(workflows, PAGE_SIZE);

  return (
    <Stack gap="normal">
      <div className="hub-toolbar">
        <div className="wf-run-hint">
          A fixed sequence of steps, each an ordinary turn by the agent you pick. It runs in the
          background, in a session of its own — started by the agent when a request matches its
          description, by you with a brief, or by a trigger.
        </div>
        {!adding && !editing && <Button onClick={startAdd} variant="primary" size="small">+ Add</Button>}
      </div>

      {adding && <WorkflowForm initial={template} onSave={f => { setTemplate(null); save(f); }} onCancel={closeForm} agents={agents} />}
      {editing && (
        <WorkflowForm
          initial={{
            name: editing.name,
            description: editing.description || '',
            steps: editing.steps || [],
            budget: editing.budget || {},
          }}
          onSave={save}
          onCancel={cancel}
          onDelete={() => { remove(editing.id); cancel(); }}
          agents={agents}
        />
      )}

      {!adding && !editing && <div className={page.count > 1 ? 'hub-paged' : undefined}>
        <div className="Box">
          {page.items.map(w => (
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
                <Button onClick={() => setRunning(w)} size="small" variant="invisible" leadingVisual={PlayIcon}
                  title="Run it, with a brief, into a conversation of your choice">
                  Run…
                </Button>
                <Button onClick={() => setTriggersFor(w)} size="small" variant="invisible" leadingVisual={ZapIcon}
                  title="Run it on a schedule or from a webhook">Triggers</Button>
                <Button onClick={() => startEdit(w)} size="small" variant="invisible">Edit</Button>
              </div>
            </div>
          ))}
          {workflows.length === 0 && (
            <Blankslate>
              <Blankslate.Description>
                No workflows yet. A workflow runs a fixed sequence of agents on one session — plan, then execute,
                then verify, each on the model you choose for it. Start from a shape:
              </Blankslate.Description>
              <div className="wf-templates">
                {TEMPLATES.map(t => (
                  <Button key={t.key} size="small" onClick={() => { setTemplate(t.form()); startAdd(); }}>{t.label}</Button>
                ))}
              </div>
            </Blankslate>
          )}
        </div>
        {page.count > 1 && (
          <Table.Pagination aria-label="Workflow pages" pageSize={PAGE_SIZE} totalCount={workflows.length}
            defaultPageIndex={page.index} onChange={({ pageIndex }) => page.setIndex(pageIndex)} />
        )}
      </div>}

      {running && <RunDialog workflow={running} sessionId={sessionId} onClose={() => setRunning(null)} />}
      {triggersFor && <TriggersDialog workflowId={triggersFor.id} workflowName={triggersFor.name} sessionId={sessionId} onClose={() => setTriggersFor(null)} />}
    </Stack>
  );
}

