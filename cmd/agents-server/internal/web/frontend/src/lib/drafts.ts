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

export const loadSessionSandbox = (sessionId: string): string => loadKey('sandbox', sessionId);
export const saveSessionSandbox = (sessionId: string, sandboxId: string): void => saveKey('sandbox', sessionId, sandboxId);

export function clearSessionPrefs(sessionId: string): void {
  saveKey('draft', sessionId, '');
  saveKey('agent', sessionId, '');
  saveKey('sandbox', sessionId, '');
}
