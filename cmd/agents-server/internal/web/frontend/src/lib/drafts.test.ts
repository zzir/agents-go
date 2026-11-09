// @vitest-environment jsdom
import { beforeEach, describe, expect, it } from 'vitest';
import { adoptNewSessionPrefs, loadSessionAgent, loadSessionWorkspace, saveSessionAgent, saveSessionWorkspace } from '@/lib/drafts';

beforeEach(() => { localStorage.clear(); });

describe('adoptNewSessionPrefs', () => {
  it('moves the New composer picks to the session its first message made', () => {
    saveSessionAgent('', 'agent-1');
    saveSessionWorkspace('', 'ws-1');
    adoptNewSessionPrefs('s1');
    expect(loadSessionAgent('s1')).toBe('agent-1');
    expect(loadSessionWorkspace('s1')).toBe('ws-1');
    // The next New starts clean: nothing inherits the workspace.
    expect(loadSessionAgent('')).toBe('');
    expect(loadSessionWorkspace('')).toBe('');
  });

  it('keeps what the session already drafted when New picked nothing', () => {
    saveSessionWorkspace('s2', 'kept');
    adoptNewSessionPrefs('s2');
    expect(loadSessionWorkspace('s2')).toBe('kept');
    expect(loadSessionAgent('s2')).toBe('');
  });
});
