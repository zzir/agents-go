import { describe, it, expect, vi } from 'vitest';

// The panel's module pulls in Primer (CSS the node loader cannot import) and
// the API layer; the helpers under test are pure.
vi.mock('@primer/react', () => ({}));
vi.mock('@primer/react/experimental', () => ({}));
vi.mock('@/lib/hooks', () => ({ useApi: () => ({}), useCrud: () => ({}) }));
vi.mock('@/lib/api', () => ({ api: {} }));
import { APPROVABLE_TOOLS, CONFIG_GROUPS, flattenConfig, nestConfig, parseApproveTools, toggleApproveTool } from '@/features/agents/AgentConfigPanel';

describe('flattenConfig / nestConfig', () => {
  // A distinct value per grouped key, so a key that fell out or landed in the
  // wrong group would show.
  const nested: Record<string, unknown> = { id: 'a1', name: 'coder', model: 'm', provider_id: 'p', instructions: 'do' };
  for (const [group, keys] of Object.entries(CONFIG_GROUPS)) {
    nested[group] = Object.fromEntries(keys.map(k => [k, `${group}.${k}`]));
  }

  it('lifts every grouped key to the top and folds each back into its group', () => {
    const flat = flattenConfig(nested);
    for (const [group, keys] of Object.entries(CONFIG_GROUPS)) {
      // The guardrails group holds a key of the same name, so the group's
      // slot legitimately carries that key's value.
      if (!keys.includes(group)) expect(flat[group]).toBeUndefined();
      for (const k of keys) expect(flat[k]).toBe(`${group}.${k}`);
    }
    expect(flat.name).toBe('coder');
    expect(nestConfig(flat)).toEqual(nested);
  });

  it('names each key in exactly one group', () => {
    const all = Object.values(CONFIG_GROUPS).flat();
    expect(new Set(all).size).toBe(all.length);
  });

  // A group the server omitted, or a key it never set, reads as unset — never
  // as a thrown error or a stray empty group on the form.
  it('tolerates a missing group and an undefined key', () => {
    const flat = flattenConfig({ name: 'x', behavior: { max_turns: 3 } });
    expect(flat).toEqual({ name: 'x', max_turns: 3 });
    expect(flattenConfig(undefined)).toEqual({});
    const back = nestConfig(flat);
    expect(back.behavior).toEqual({ max_turns: 3 });
    expect(back.resilience).toEqual({});
    expect(back.max_turns).toBeUndefined();
  });
});

describe('approve tools checklist', () => {
  it('reads the stored list, and refuses what a checklist cannot show', () => {
    expect(parseApproveTools('')).toEqual([]);
    expect(parseApproveTools('  ')).toEqual([]);
    expect(parseApproveTools('["exec_command","*"]')).toEqual(['exec_command', '*']);
    expect(parseApproveTools('{"a":1}')).toBeNull();
    expect(parseApproveTools('[1]')).toBeNull();
    expect(parseApproveTools('not json')).toBeNull();
  });

  it('toggles one name in place and stores an emptied list as unset', () => {
    expect(toggleApproveTool('', 'exec_command', true)).toBe('["exec_command"]');
    expect(toggleApproveTool('["exec_command"]', 'exec_command', true)).toBe('["exec_command"]');
    expect(toggleApproveTool('["exec_command","srv__tool"]', 'exec_command', false)).toBe('["srv__tool"]');
    expect(toggleApproveTool('["exec_command"]', 'exec_command', false)).toBe('');
  });

  // "*" joins the list rather than replacing it, so switching it off again
  // restores the names that were checked before.
  it('keeps the checked names under "every tool"', () => {
    const all = toggleApproveTool('["exec_command"]', '*', true);
    expect(parseApproveTools(all)).toEqual(['exec_command', '*']);
    expect(toggleApproveTool(all, '*', false)).toBe('["exec_command"]');
  });

  it('lists each built-in name once', () => {
    const names = APPROVABLE_TOOLS.flatMap(g => g.tools);
    expect(new Set(names).size).toBe(names.length);
    expect(names).toContain('exec_command');
  });
});
