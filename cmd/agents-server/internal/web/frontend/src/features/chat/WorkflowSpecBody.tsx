import { Label } from '@primer/react';
import { api } from '@/lib/api';
import { useApi } from '@/lib/hooks';
import { diffLines } from '@/lib/diff';
import { EdgeGraph, type Workflow } from '@/features/workflows/graph';
import { canonicalWorkflowText, specFromStored, specSteps, stepFlags, type WorkflowSpec } from '@/lib/workflowArgs';

// The save_workflow card's body: the proposed definition — description, steps,
// the sequence drawn — and, while the save awaits approval, what it replaces:
// the stored definition of that name diffed line by line, or "new".
export function WorkflowSpecBody({ spec, pending }: { spec: WorkflowSpec; pending: boolean }) {
  const b = spec.budget;
  const bounds = ([['steps', b.max_steps], ['tokens', b.max_tokens], ['minutes', b.max_minutes], ['laps', b.max_laps]] as const)
    .filter(([, v]) => v > 0).map(([unit, v]) => `${v} ${unit}`);
  return (
    <div className="ToolCallCard-wf">
      {pending && <WorkflowSaveReview spec={spec} />}
      {spec.description && <div className="ToolCallCard-wf-desc">{spec.description}</div>}
      <ol className="ToolCallCard-wf-steps">
        {spec.steps.map((s, i) => (
          <li key={i} className="ToolCallCard-wf-step">
            <div className="ToolCallCard-wf-step-head">
              <span className="ToolCallCard-wf-step-name">{s.name || `Step ${i + 1}`}</span>
              <span className="ToolCallCard-wf-step-agent">{s.agent}</span>
              {stepFlags(s).map(f => <Label key={f} variant="secondary" size="small">{f}</Label>)}
            </div>
            {s.prompt && <pre className="ToolCallCard-wf-prompt">{s.prompt}</pre>}
          </li>
        ))}
      </ol>
      {spec.steps.length > 0 && <EdgeGraph steps={specSteps(spec)} always />}
      {bounds.length > 0 && <div className="ToolCallCard-wf-budget">Budget per run: {bounds.join(' · ')}</div>}
    </div>
  );
}

interface AgentRef { id: string; name: string }

// WorkflowSaveReview says what the save does to what exists — mounted only
// while the approval is pending, since afterwards the store holds the result.
function WorkflowSaveReview({ spec }: { spec: WorkflowSpec }) {
  // An empty list arrives as null, so "loaded" is the loading flag, not the data.
  const { data: workflows, loading, error } = useApi<Workflow[] | null>(() => api.workflows.list() as Promise<Workflow[] | null>);
  const { data: agents, loading: agentsLoading } = useApi<AgentRef[] | null>(() => api.agents.list() as Promise<AgentRef[] | null>);
  // Both, before a line is drawn: a diff over agent ids would flash every
  // step's agent line as a change until the names arrive.
  if (loading || agentsLoading) return null;
  if (error) {
    // Not "new workflow": the store was not read, so what the save replaces
    // is unknown — say so rather than review against nothing.
    return <div className="ToolCallCard-wf-review ToolCallCard-wf-review-error">Could not load the stored workflows to review against: {error}</div>;
  }
  const name = spec.name.trim().toLowerCase();
  const existing = (workflows || []).find(w => (w.name || '').trim().toLowerCase() === name);
  if (!existing) {
    return <div className="ToolCallCard-wf-review"><Label variant="success" size="small">new workflow</Label></div>;
  }
  const agentName = (id: string) => (agents || []).find(a => a.id === id)?.name || id;
  const diff = diffLines(canonicalWorkflowText(specFromStored(existing, agentName)), canonicalWorkflowText(spec));
  const changed = diff.some(l => l.type !== 'same');
  const from = (existing.steps || []).length;
  return (
    <div className="ToolCallCard-wf-review">
      <div className="ToolCallCard-wf-review-head">
        <Label variant="attention" size="small">replaces {existing.name}</Label>
        <span>{from === spec.steps.length ? `${from} steps` : `${from} → ${spec.steps.length} steps`}{changed ? '' : ' · no change'}</span>
      </div>
      {changed && (
        <div className="ToolCallCard-wf-diff">
          {diff.map((l, i) => (
            <div key={i} className={'ToolCallCard-wf-diff-line ToolCallCard-wf-diff-' + l.type}>
              <span className="ToolCallCard-wf-diff-sign">{l.type === 'add' ? '+' : l.type === 'del' ? '-' : ' '}</span>
              {l.text}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
