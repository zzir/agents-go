import type { Workflow, WorkflowStep } from '@/features/workflows/graph';

// The save_workflow tool's arguments: a definition as the model writes it,
// steps, agents and edges by NAME (mirrors bridge.workflowSpec). The approval
// card renders it, and brings the stored definition it would replace to the
// same shape so the two can be diffed line by line.

export interface WorkflowSpecStep {
  name: string;
  agent: string;
  prompt: string;
  gate: boolean;
  gate_pass: string;
  gate_fail: string;
  pause_before: boolean;
  compact_before: boolean;
  on_success: string;
  on_failure: string;
}

export interface WorkflowSpec {
  name: string;
  description: string;
  steps: WorkflowSpecStep[];
  budget: { max_steps: number; max_tokens: number; max_minutes: number; max_laps: number };
}

const str = (v: unknown) => (typeof v === 'string' ? v : '');
const bool = (v: unknown) => v === true;
const num = (v: unknown) => (typeof v === 'number' && Number.isFinite(v) ? v : 0);

// parseWorkflowSpec reads the tool call's arguments defensively: a field the
// model left out reads as empty, and anything but an object with a name is null.
export function parseWorkflowSpec(argsJSON: string): WorkflowSpec | null {
  let raw: unknown;
  try {
    raw = JSON.parse(argsJSON);
  } catch {
    return null;
  }
  if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return null;
  const o = raw as Record<string, unknown>;
  if (typeof o.name !== 'string') return null;
  const budget = (o.budget && typeof o.budget === 'object' ? o.budget : {}) as Record<string, unknown>;
  const steps = Array.isArray(o.steps) ? o.steps : [];
  return {
    name: o.name,
    description: str(o.description),
    steps: steps
      .filter(s => s && typeof s === 'object')
      .map((s: Record<string, unknown>) => ({
        name: str(s.name), agent: str(s.agent), prompt: str(s.prompt),
        gate: bool(s.gate), gate_pass: str(s.gate_pass), gate_fail: str(s.gate_fail),
        pause_before: bool(s.pause_before), compact_before: bool(s.compact_before),
        on_success: str(s.on_success), on_failure: str(s.on_failure),
      })),
    budget: {
      max_steps: num(budget.max_steps), max_tokens: num(budget.max_tokens),
      max_minutes: num(budget.max_minutes), max_laps: num(budget.max_laps),
    },
  };
}

// storedStepNames names every step of a stored definition, uniquely: its own
// name, or "Step N" for a nameless one (suffixed if that collides) — the same
// rule the server's get_workflow applies, so a diff against a save the model
// read back shows no phantom rename.
export function storedStepNames(steps: WorkflowStep[]): string[] {
  const used = new Set<string>();
  const names = steps.map(s => {
    const n = (s.name || '').trim();
    if (n) used.add(n.toLowerCase());
    return n;
  });
  steps.forEach((_, i) => {
    if (names[i]) return;
    let n = `Step ${i + 1}`;
    for (let k = 2; used.has(n.toLowerCase()); k++) n = `Step ${i + 1} (${k})`;
    names[i] = n;
    used.add(n.toLowerCase());
  });
  return names;
}

// specFromStored is a stored definition in the tool's shape: ids become names,
// an agent that no longer exists keeps its id.
export function specFromStored(w: Workflow, agentName: (id: string) => string): WorkflowSpec {
  const steps = w.steps || [];
  const names = storedStepNames(steps);
  const byId = new Map<string, string>();
  steps.forEach((s, i) => { if (s.id) byId.set(s.id, names[i]); });
  const edge = (t?: string) => (t ? byId.get(t) ?? t : '');
  return {
    name: w.name,
    description: w.description || '',
    steps: steps.map((s, i) => ({
      name: names[i], agent: agentName(s.agent_config_id), prompt: s.prompt || '',
      gate: !!s.gate, gate_pass: s.gate?.pass || '', gate_fail: s.gate?.fail || '',
      pause_before: !!s.pause_before, compact_before: !!s.compact_before,
      on_success: edge(s.on_success), on_failure: edge(s.on_failure),
    })),
    budget: {
      max_steps: w.budget?.max_steps || 0, max_tokens: w.budget?.max_tokens || 0,
      max_minutes: w.budget?.max_minutes || 0, max_laps: w.budget?.max_laps || 0,
    },
  };
}

// specSteps is the spec's steps in the graph's shape — the step name standing
// in for the id, which is what the spec's edges name — so EdgeGraph draws a
// proposal exactly as it draws a stored definition.
export function specSteps(spec: WorkflowSpec): WorkflowStep[] {
  return spec.steps.map(s => ({
    id: s.name, name: s.name, agent_config_id: s.agent, prompt: s.prompt,
    gate: s.gate ? { pass: s.gate_pass, fail: s.gate_fail } : null,
    pause_before: s.pause_before, compact_before: s.compact_before,
    on_success: s.on_success, on_failure: s.on_failure,
  }));
}

// stepFlags is a step's shape in words, in the order the card shows them.
export function stepFlags(s: WorkflowSpecStep): string[] {
  const out: string[] = [];
  if (s.gate) out.push(`gate ${s.gate_pass || 'PASS'}/${s.gate_fail || 'FAIL'}`);
  if (s.pause_before) out.push('pause before');
  if (s.compact_before) out.push('compact before');
  if (s.on_success) out.push(`${s.gate ? 'PASS' : 'ok'} → ${s.on_success}`);
  if (s.on_failure) out.push(`${s.gate ? 'FAIL' : 'error'} → ${s.on_failure}`);
  return out;
}

// canonicalWorkflowText lays a spec out one fact per line — the head, the
// budget, then each step's line and its prompt indented — for a line diff
// between the stored definition and the proposed one.
export function canonicalWorkflowText(spec: WorkflowSpec): string {
  const lines = [`name: ${spec.name}`, `description: ${spec.description}`];
  const b = spec.budget;
  const bounds = ([['max_steps', b.max_steps], ['max_tokens', b.max_tokens], ['max_minutes', b.max_minutes], ['max_laps', b.max_laps]] as const)
    .filter(([, v]) => v > 0).map(([k, v]) => `${k} ${v}`);
  lines.push(`budget: ${bounds.length ? bounds.join(' · ') : 'none'}`);
  for (const s of spec.steps) {
    lines.push([`step ${s.name}`, `agent ${s.agent}`, ...stepFlags(s)].join(' · '));
    for (const p of s.prompt.split('\n')) lines.push('  ' + p);
  }
  return lines.join('\n');
}
