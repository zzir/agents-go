import type { Workflow, WorkflowStep } from '@/features/workflows/graph';

// The save_workflow tool's arguments: a definition as the model writes it, steps,
// agents and edges by NAME (mirrors bridge.workflowSpec). The approval card
// renders it and diffs it line by line against the stored definition in the same shape.

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

const str = (v: unknown) => (typeof v === 'string' ? v.trim() : '');
const bool = (v: unknown) => v === true;
const num = (v: unknown) => (typeof v === 'number' && Number.isFinite(v) ? v : 0);

// END is the reserved edge target (mirrors store.WorkflowStepEnd; graph.END is
// the same string, kept here so this module has no dependency on the chart).
const END = 'end';

// GATE_TRIM is what the server's Verdict strips off a candidate line and off a
// configured word (store.normalizeGateWord); the card shows the word as stored
// and matched.
const GATE_TRIM = '*_`.!:';
export function normalizeGateWord(w: string): string {
  let t = w.trim();
  let a = 0;
  let b = t.length;
  while (a < b && GATE_TRIM.includes(t[a])) a++;
  while (b > a && GATE_TRIM.includes(t[b - 1])) b--;
  t = t.slice(a, b);
  return t.trim();
}

// parseWorkflowSpec reads the tool call's arguments defensively (a missing field
// is empty, anything but an object with a name is null) into the shape the server
// STORES: bridge.resolveWorkflowSpec + store.NormalizeWorkflow, mirrored here.
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
  const rawSteps = Array.isArray(o.steps) ? o.steps : [];
  const steps: WorkflowSpecStep[] = rawSteps
    .filter(s => s && typeof s === 'object')
    .map((s: Record<string, unknown>) => ({
      name: str(s.name), agent: str(s.agent), prompt: str(s.prompt),
      gate: bool(s.gate), gate_pass: normalizeGateWord(str(s.gate_pass)), gate_fail: normalizeGateWord(str(s.gate_fail)),
      pause_before: bool(s.pause_before), compact_before: bool(s.compact_before),
      on_success: str(s.on_success), on_failure: str(s.on_failure),
    }));
  // Edges after every name is known, so one may name a step in either direction.
  const byLower = new Map<string, string>();
  for (const s of steps) if (s.name && !byLower.has(s.name.toLowerCase())) byLower.set(s.name.toLowerCase(), s.name);
  const edge = (t: string) => {
    const k = t.toLowerCase();
    if (!k) return '';
    if (k === END) return END;
    return byLower.get(k) ?? t; // an unknown target stays as written: the server refuses it
  };
  for (const s of steps) {
    s.on_success = edge(s.on_success);
    s.on_failure = edge(s.on_failure);
  }
  return {
    name: o.name.trim(),
    description: str(o.description),
    steps,
    budget: {
      max_steps: num(budget.max_steps), max_tokens: num(budget.max_tokens),
      max_minutes: num(budget.max_minutes), max_laps: num(budget.max_laps),
    },
  };
}

// storedStepNames names every step of a stored definition uniquely ("Step N" for
// a nameless one, suffixed on collision), by the rule the server's get_workflow
// applies, so a diff against a save the model read back shows no phantom rename.
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

// specSteps is the spec's steps in the graph's shape, the step name standing in
// for the id (which the spec's edges name), so EdgeGraph draws a proposal like a
// stored definition.
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

// canonicalWorkflowText lays a spec out one fact per line (head, budget, each
// step's line and indented prompt) for a line diff against the stored definition.
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
