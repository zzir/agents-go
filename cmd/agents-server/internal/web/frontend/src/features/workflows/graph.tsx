import { useMermaidSvg } from '@/features/chat/MermaidBlock';
import './graph.css';

// A workflow definition as the API carries it, and the sequence drawn: shared
// by the hub (the editor and its rows) and the chat's save_workflow card, which
// draws a proposed definition the same way.

// One step: the agent that runs it and the prompt that starts its turn. The id
// is server-assigned and stable, so inserting a step never renumbers what a run
// in flight names.
export interface WorkflowStep {
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
  // Where the sequence goes next: empty on_success falls through to the next step,
  // empty on_failure fails the workflow, "end" stops, a backward edge loops.
  on_success?: string;
  on_failure?: string;
}

// What one execution may spend before it is stopped; 0/absent = no bound,
// except max_laps (times one execution may take the same backward edge), whose
// absent is the server's default of 3.
export interface WorkflowBudget {
  max_steps?: number;
  max_tokens?: number;
  max_minutes?: number;
  max_laps?: number;
}

export interface Workflow {
  id: string;
  name: string;
  description?: string;
  steps: WorkflowStep[];
  budget?: WorkflowBudget;
  scope?: string;
  owner_id?: string;
}

// END is the reserved edge target that stops the execution (mirrors
// store.WorkflowStepEnd).
export const END = 'end';

export const stepLabel = (s: WorkflowStep, i: number) => (s.name || '').trim() || `Step ${i + 1}`;

// branches reports a sequence with a shape a list does not show: a named edge,
// or a check.
export const branches = (steps: WorkflowStep[]) => steps.some(s => s.on_success || s.on_failure || s.gate);

// How the sequence is drawn: by default only a branching one is; `always` draws a
// linear one too, as a chain. label names a step's node.
export interface GraphOpts {
  always?: boolean;
  label?: (s: WorkflowStep, i: number) => string;
}

// edgeSummary is the sequence's shape in words.
export function edgeSummary(steps: WorkflowStep[], { always, label = stepLabel }: GraphOpts = {}): string[] {
  const nameOf = (id?: string, i?: number) => {
    if (id === END) return 'end';
    if (!id) return i !== undefined && i + 1 < steps.length ? label(steps[i + 1], i + 1) : 'end';
    const j = steps.findIndex(s => s.id === id);
    return j >= 0 ? label(steps[j], j) : id;
  };
  if (!branches(steps)) return always && steps.length ? [[...steps.map(label), 'end'].join(' → ')] : [];
  return steps.map((s, i) => {
    const ok = `${s.gate ? 'PASS' : 'ok'} → ${nameOf(s.on_success, i)}`;
    const bad = `${s.gate ? 'FAIL' : 'error'} → ${s.on_failure ? nameOf(s.on_failure) : 'stop, failed'}`;
    return `${label(s, i)}: ${ok} · ${bad}`;
  });
}

// edgeGraph is the sequence as a flowchart: a node per step, a solid success
// edge, a dotted failure edge, two terminals; a linear sequence is one chain.
// Empty when nothing to draw, like edgeSummary.
export function edgeGraph(steps: WorkflowStep[], { always, label = stepLabel }: GraphOpts = {}): string {
  const q = (t: string) => '"' + t.replace(/"/g, '#quot;') + '"';
  const box = (i: number) => `n${i}[${q(label(steps[i], i))}]`;
  if (!branches(steps)) {
    return always && steps.length ? `flowchart LR\n  ${[...steps.map((_, i) => box(i)), 'END((end))'].join(' --> ')}` : '';
  }
  const node = (id?: string, i?: number): string => {
    if (id === END) return 'END';
    if (!id) return i !== undefined && i + 1 < steps.length ? `n${i + 1}` : 'END';
    const j = steps.findIndex(s => s.id === id);
    return j >= 0 ? `n${j}` : 'END';
  };
  const lines = ['flowchart LR'];
  steps.forEach((_, i) => lines.push('  ' + box(i)));
  lines.push('  END((end))', '  FAILED((failed))');
  steps.forEach((s, i) => {
    lines.push(`  n${i} -->|${s.gate ? 'PASS' : 'ok'}| ${node(s.on_success, i)}`);
    lines.push(`  n${i} -.->|${s.gate ? 'FAIL' : 'error'}| ${s.on_failure ? node(s.on_failure) : 'FAILED'}`);
  });
  return lines.join('\n');
}

// EdgeGraph draws the sequence; until mermaid has rendered (or if it cannot)
// the same shape stands in words.
export function EdgeGraph({ steps, ...opts }: { steps: WorkflowStep[] } & GraphOpts) {
  const { svg, failed } = useMermaidSvg(edgeGraph(steps, opts));
  const summary = edgeSummary(steps, opts);
  if (svg && !failed) {
    return <div className="wf-edge-graph" aria-label={summary.join('; ')} dangerouslySetInnerHTML={{ __html: svg }} />;
  }
  return (
    <div className="wf-edge-summary">
      {summary.map(line => <div key={line}>{line}</div>)}
    </div>
  );
}
