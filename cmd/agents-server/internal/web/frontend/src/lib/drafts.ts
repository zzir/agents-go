function loadKey(kind: string, sessionId: string): string {
  try {
    return localStorage.getItem(`chat.${kind}.${sessionId}`) || '';
  } catch {
    return '';
  }
}

function saveKey(kind: string, sessionId: string, value: string): void {
  try {
    const key = `chat.${kind}.${sessionId}`;
    if (value) localStorage.setItem(key, value);
    else localStorage.removeItem(key);
  } catch {
    // Ignore write errors (private browsing, quota exceeded, etc.)
  }
}

export const loadDraft = (sessionId: string): string => loadKey('draft', sessionId);
export const saveDraft = (sessionId: string, text: string): void => saveKey('draft', sessionId, text);
export const clearDraft = (sessionId: string): void => saveKey('draft', sessionId, '');

export const loadSessionAgent = (sessionId: string): string => loadKey('agent', sessionId);
export const saveSessionAgent = (sessionId: string, agentConfigId: string): void => saveKey('agent', sessionId, agentConfigId);

// The user's pre-binding project choice; once the first run binds the session,
// the server value wins and this draft stops mattering.
export const loadSessionProject = (sessionId: string): string => loadKey('project', sessionId);
export const saveSessionProject = (sessionId: string, projectId: string): void => saveKey('project', sessionId, projectId);

export function clearSessionPrefs(sessionId: string): void {
  saveKey('draft', sessionId, '');
  saveKey('agent', sessionId, '');
  saveKey('project', sessionId, '');
}
