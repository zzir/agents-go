import { describe, it, expect } from 'vitest';
import { diffLines } from '@/lib/diff';
import {
  canonicalWorkflowText, parseWorkflowSpec, specFromStored, specSteps, stepFlags, storedStepNames,
} from '@/lib/workflowArgs';
import type { Workflow } from '@/features/workflows/graph';

const args = JSON.stringify({
  name: 'build',
  description: 'Implement a feature end to end',
  steps: [
    { name: 'plan', agent: 'planner', prompt: 'Write a plan.', gate: false, gate_pass: '', gate_fail: '', pause_before: false, compact_before: false, on_success: '', on_failure: '' },
    { name: 'exec', agent: 'coder', prompt: 'Carry out the plan.\nAdd tests.', gate: false, gate_pass: '', gate_fail: '', pause_before: false, compact_before: false, on_success: '', on_failure: '' },
    { name: 'verify', agent: 'reviewer', prompt: 'Run the tests.', gate: true, gate_pass: '', gate_fail: '', pause_before: true, compact_before: true, on_success: 'end', on_failure: 'exec' },
  ],
  budget: { max_steps: 0, max_tokens: 0, max_minutes: 0, max_laps: 2 },
});

// The same definition as the API stores it: ids, a nameless first step, edges
// by id, agent ids.
const stored: Workflow = {
  id: 'wf1', name: 'build', description: 'Implement a feature end to end',
  steps: [
    { id: 's1', name: '', agent_config_id: 'a-plan', prompt: 'Write a plan.' },
    { id: 's2', name: 'exec', agent_config_id: 'a-code', prompt: 'Carry out the plan.\nAdd tests.' },
    { id: 's3', name: 'verify', agent_config_id: 'a-rev', prompt: 'Run the tests.', gate: {}, pause_before: true, compact_before: true, on_success: 'end', on_failure: 's2' },
  ],
  budget: { max_laps: 2 },
};
const agentName = (id: string) => ({ 'a-plan': 'planner', 'a-code': 'coder', 'a-rev': 'reviewer' } as Record<string, string>)[id] || id;

describe('parseWorkflowSpec', () => {
  it('reads the tool arguments', () => {
    const spec = parseWorkflowSpec(args)!;
    expect(spec.name).toBe('build');
    expect(spec.steps.map(s => s.name)).toEqual(['plan', 'exec', 'verify']);
    expect(spec.steps[2]).toMatchObject({ gate: true, pause_before: true, on_success: 'end', on_failure: 'exec' });
    expect(spec.budget.max_laps).toBe(2);
  });

  it('tolerates missing fields and refuses non-objects', () => {
    expect(parseWorkflowSpec('{"name":"x"}')).toEqual({
      name: 'x', description: '', steps: [], budget: { max_steps: 0, max_tokens: 0, max_minutes: 0, max_laps: 0 },
    });
    expect(parseWorkflowSpec('{"name":')).toBeNull();
    expect(parseWorkflowSpec('[]')).toBeNull();
    expect(parseWorkflowSpec('{"steps":[]}')).toBeNull();
  });
});

describe('storedStepNames', () => {
  it('names a nameless step by its position, uniquely', () => {
    expect(storedStepNames(stored.steps)).toEqual(['Step 1', 'exec', 'verify']);
    expect(storedStepNames([
      { name: 'Step 2', agent_config_id: 'a', prompt: '' },
      { name: '', agent_config_id: 'a', prompt: '' },
    ])).toEqual(['Step 2', 'Step 2 (2)']);
  });
});

describe('specFromStored / specSteps', () => {
  it('brings a stored definition to the tool shape: names for ids', () => {
    const spec = specFromStored(stored, agentName);
    expect(spec.steps.map(s => s.name)).toEqual(['Step 1', 'exec', 'verify']);
    expect(spec.steps.map(s => s.agent)).toEqual(['planner', 'coder', 'reviewer']);
    expect(spec.steps[2].on_failure).toBe('exec');
    expect(spec.steps[2].on_success).toBe('end');
    expect(spec.steps[2].gate).toBe(true);
    expect(spec.budget).toEqual({ max_steps: 0, max_tokens: 0, max_minutes: 0, max_laps: 2 });
  });

  it('draws a proposal with the step name as the id the edges name', () => {
    const steps = specSteps(parseWorkflowSpec(args)!);
    expect(steps[2]).toMatchObject({ id: 'verify', on_failure: 'exec', gate: { pass: '', fail: '' } });
    expect(steps[0].gate).toBeNull();
  });
});

describe('canonicalWorkflowText', () => {
  it('is the same text for the proposal and the stored definition it matches', () => {
    const proposal = canonicalWorkflowText(parseWorkflowSpec(args)!);
    const before = canonicalWorkflowText(specFromStored(stored, agentName));
    // The nameless stored step is "Step 1", the proposal calls it "plan": the
    // one real difference.
    const d = diffLines(before, proposal).filter(l => l.type !== 'same');
    expect(d).toEqual([
      { type: 'del', text: 'step Step 1 · agent planner' },
      { type: 'add', text: 'step plan · agent planner' },
    ]);
    expect(proposal.split('\n')).toEqual([
      'name: build',
      'description: Implement a feature end to end',
      'budget: max_laps 2',
      'step plan · agent planner',
      '  Write a plan.',
      'step exec · agent coder',
      '  Carry out the plan.',
      '  Add tests.',
      'step verify · agent reviewer · gate PASS/FAIL · pause before · compact before · PASS → end · FAIL → exec',
      '  Run the tests.',
    ]);
  });

  it('a changed prompt diffs as that line alone', () => {
    const a = parseWorkflowSpec(args)!;
    const b = parseWorkflowSpec(args)!;
    b.steps[2].prompt = 'Run the tests and lint.';
    const d = diffLines(canonicalWorkflowText(a), canonicalWorkflowText(b)).filter(l => l.type !== 'same');
    expect(d).toEqual([
      { type: 'del', text: '  Run the tests.' },
      { type: 'add', text: '  Run the tests and lint.' },
    ]);
  });

  it('flags read in the card order', () => {
    expect(stepFlags({ name: 'v', agent: 'r', prompt: '', gate: true, gate_pass: 'LGTM', gate_fail: 'NOPE', pause_before: false, compact_before: false, on_success: '', on_failure: 'fix' }))
      .toEqual(['gate LGTM/NOPE', 'FAIL → fix']);
  });
});
