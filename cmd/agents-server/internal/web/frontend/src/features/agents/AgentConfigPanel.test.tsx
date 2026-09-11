import { describe, it, expect, vi } from 'vitest';

// The panel's module pulls in Primer (CSS the node loader cannot import) and
// the API layer; the helpers under test are pure.
vi.mock('@primer/react', () => ({}));
vi.mock('@primer/react/experimental', () => ({}));
vi.mock('@/lib/hooks', () => ({ useApi: () => ({}), useCrud: () => ({}) }));
vi.mock('@/lib/api', () => ({ api: {} }));
import { APPROVABLE_TOOLS, parseApproveTools, toggleApproveTool } from '@/features/agents/AgentConfigPanel';

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
