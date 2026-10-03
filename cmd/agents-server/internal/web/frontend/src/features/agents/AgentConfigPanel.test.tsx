import { describe, it, expect, vi } from 'vitest';

// The panel's module pulls in Primer (CSS the node loader cannot import) and
// the API layer; the helpers under test are pure.
vi.mock('@primer/react', () => ({}));
vi.mock('@primer/react/experimental', () => ({}));
vi.mock('@/lib/hooks', () => ({ useApi: () => ({}), useCrud: () => ({}) }));
vi.mock('@/lib/api', () => ({ api: {} }));
import { APPROVAL_MODES, CONFIG_GROUPS, approvalListHint, approvalSuggestions, danglingNote, danglingRefs, flattenConfig, initialApproval, legacyFallbackProvider, modelPrefill, nestConfig, resolveFallbackEntry, toggleListEntry } from '@/features/agents/AgentConfigPanel';

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

  // The checklist switch is the behavior group's: read from it, saved into it.
  it('reads and writes the checklist switch as behavior.checklist', () => {
    expect(flattenConfig({ name: 'x', behavior: { checklist: true } }).checklist).toBe(true);
    expect(flattenConfig({ name: 'x', behavior: {} }).checklist).toBeUndefined();
    expect(nestConfig({ name: 'x', checklist: true }).behavior).toEqual({ checklist: true });
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

describe('toggleListEntry', () => {
  it('toggles one id in place, once', () => {
    expect(toggleListEntry([], 'a', true)).toEqual(['a']);
    expect(toggleListEntry(['a'], 'a', true)).toEqual(['a']);
    expect(toggleListEntry(['a', 'b'], 'a', false)).toEqual(['b']);
  });
});

describe('approval modes', () => {
  const tools = [
    { name: 'read_file', read_only: true, source: 'sandbox' },
    { name: 'write_file', source: 'sandbox' },
    { name: 'exec_command', source: 'sandbox' },
    { name: 'docs__search', read_only: true, source: 'mcp:docs' },
    { name: 'submit_plan', source: 'plan' },
  ];

  it('offers the three modes', () => {
    expect(APPROVAL_MODES.map(([v]) => v)).toEqual(['never', 'on_change', 'always']);
  });

  // A row from before the field reads as never; one whose list said "*"
  // reads as always, and the mode replaces the "*".
  it('reads a stored agent into a mode', () => {
    expect(initialApproval(undefined)).toEqual({ approval_mode: 'never', approve_tools: [] });
    expect(initialApproval({ approve_tools: ['write_file'] })).toEqual({ approval_mode: 'never', approve_tools: ['write_file'] });
    expect(initialApproval({ approve_tools: ['*', 'exec_command'] })).toEqual({ approval_mode: 'always', approve_tools: ['exec_command'] });
    expect(initialApproval({ approval_mode: 'on_change', approve_tools: [] })).toEqual({ approval_mode: 'on_change', approve_tools: [] });
  });

  // The list can only add a question: under never every tool is on offer,
  // under on_change only the reads the mode lets through, under always
  // nothing. submit_plan asks by itself and is never offered.
  it('suggests what the mode does not already ask about', () => {
    expect(approvalSuggestions(tools, 'never', ['write_file'])).toEqual(['read_file', 'exec_command', 'docs__search']);
    expect(approvalSuggestions(tools, 'on_change', [])).toEqual(['read_file', 'docs__search']);
    expect(approvalSuggestions(tools, 'always', [])).toEqual([]);
    expect(approvalSuggestions(null, 'never', [])).toEqual([]);
  });

  it('names the current mode in the list caption', () => {
    expect(approvalListHint('on_change')).toContain('now: Ask before changes');
    expect(approvalListHint('')).toContain('now: Never ask');
  });
});

describe('legacyFallbackProvider', () => {
  const providers = [
    { id: 'oa', type: '', base_url: '' },
    { id: 'an', type: 'anthropic', base_url: '' },
    { id: 'gw', type: 'openai', base_url: 'https://gw.example/v1' },
  ];

  // The server's own matching: "" and "openai" are one backend, a trailing
  // slash the same host, and a provider by id needs no matching at all.
  it('finds the provider at the endpoint an old entry named', () => {
    expect(legacyFallbackProvider({ model: 'm' }, providers)).toBe('oa');
    expect(legacyFallbackProvider({ provider_type: 'openai' }, providers)).toBe('oa');
    expect(legacyFallbackProvider({ provider_type: 'anthropic' }, providers)).toBe('an');
    expect(legacyFallbackProvider({ provider_type: 'openai', base_url: 'https://gw.example/v1/' }, providers)).toBe('gw');
    expect(legacyFallbackProvider({ provider_type: 'openai', base_url: 'https://other.example' }, providers)).toBeUndefined();
    expect(legacyFallbackProvider({ provider_type: 'anthropic', base_url: 'https://gw.example/v1' }, providers)).toBeUndefined();
  });
});

describe('resolveFallbackEntry', () => {
  const providers = [{ id: 'anth', type: 'anthropic', base_url: 'https://api.anthropic.com' }];
  it('a fresh entry has no endpoint yet and stays editable', () => {
    expect(resolveFallbackEntry({ provider_id: '' }, providers)).toEqual({ providerId: '', unreachable: false });
    expect(resolveFallbackEntry({}, [])).toEqual({ providerId: '', unreachable: false });
  });
  it('an entry naming a provider is that provider', () => {
    expect(resolveFallbackEntry({ provider_id: 'anth' }, providers)).toEqual({ providerId: 'anth', unreachable: false });
  });
  it('a legacy endpoint entry resolves to its match, or is unreachable', () => {
    expect(resolveFallbackEntry({ provider_type: 'anthropic', base_url: 'https://api.anthropic.com/' }, providers)).toEqual({ providerId: 'anth', unreachable: false });
    expect(resolveFallbackEntry({ provider_type: 'openai', base_url: '' }, providers)).toEqual({ providerId: '', unreachable: true });
  });
});

describe('danglingRefs', () => {
  it('is empty when every selected id is listed', () => {
    expect(danglingRefs(['m1', 'm2'], ['m2', 'm1', 'm3'])).toEqual([]);
  });

  it('names the selected ids the list lacks, in selection order', () => {
    expect(danglingRefs(['m1', 'gone', 'm2', 'lost'], ['m1', 'm2'])).toEqual(['gone', 'lost']);
  });

  it('is empty for an empty selection, whatever is listed', () => {
    expect(danglingRefs([], [])).toEqual([]);
    expect(danglingRefs([], ['m1'])).toEqual([]);
  });

  it('words the note for one and for several', () => {
    expect(danglingNote(1, 'skill')).toBe('1 selected skill no longer exists or cannot be referenced here — saving removes it.');
    expect(danglingNote(3, 'MCP server')).toBe('3 selected MCP servers no longer exist or cannot be referenced here — saving removes them.');
  });
});

describe('modelPrefill', () => {
  const claude = { id: 'claude-x', context_window: 200000, thinking_types: ['adaptive', 'enabled'] };
  const haiku = { id: 'claude-haiku', context_window: 200000, thinking_types: ['enabled'] };
  const gpt = { id: 'gpt-x' };

  it('fills an empty context window from the listing and leaves a typed one alone', () => {
    expect(modelPrefill(claude, { context_window: '', thinking_mode: '' }, 'anthropic').context_window).toBe('200000');
    expect(modelPrefill(claude, { context_window: '128000', thinking_mode: '' }, 'anthropic').context_window).toBeUndefined();
    expect(modelPrefill(gpt, { context_window: '', thinking_mode: '' }, 'openai')).toEqual({});
    expect(modelPrefill(undefined, { context_window: '', thinking_mode: '' }, 'anthropic')).toEqual({});
  });

  it('picks the thinking mode on Anthropic: a budget only for a model without adaptive thinking', () => {
    expect(modelPrefill(haiku, { context_window: '1', thinking_mode: '' }, 'anthropic').thinking_mode).toBe('budget');
    expect(modelPrefill(claude, { context_window: '1', thinking_mode: 'budget' }, 'anthropic').thinking_mode).toBe('');
    expect(modelPrefill(haiku, { context_window: '1', thinking_mode: '' }, 'openai').thinking_mode).toBeUndefined();
  });
});
