// @vitest-environment jsdom
import { beforeEach, describe, expect, it } from 'vitest';
import { adoptNewSessionPrefs, loadSessionAgent, loadSessionProject, saveSessionAgent, saveSessionProject } from '@/lib/drafts';

beforeEach(() => { localStorage.clear(); });

describe('adoptNewSessionPrefs', () => {
  it('moves the New composer picks to the session its first message made', () => {
    saveSessionAgent('', 'agent-1');
    saveSessionProject('', 'proj-1');
    adoptNewSessionPrefs('s1');
    expect(loadSessionAgent('s1')).toBe('agent-1');
    expect(loadSessionProject('s1')).toBe('proj-1');
    // The next New starts clean: nothing inherits the project.
    expect(loadSessionAgent('')).toBe('');
    expect(loadSessionProject('')).toBe('');
  });

  it('keeps what the session already drafted when New picked nothing', () => {
    saveSessionProject('s2', 'kept');
    adoptNewSessionPrefs('s2');
    expect(loadSessionProject('s2')).toBe('kept');
    expect(loadSessionAgent('s2')).toBe('');
  });
});
