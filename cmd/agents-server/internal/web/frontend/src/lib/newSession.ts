import { api } from '@/lib/api';

// The name a session is created with; the server's DefaultSessionName.
export const DEFAULT_SESSION_NAME = 'New Session';

interface SessionRow { id: string; name: string; updated_at?: string }

// createOrReuseSession is what "New session" does: it hands back the newest
// conversation that is still empty and unnamed when there is one, and only
// otherwise creates another. Every click on "+" making a row, whether or not
// the last one was ever used, is how a sidebar fills with forty "New
// Session"s that cannot be told apart — the picker and the sidebar then list
// them all. Emptiness is a one-row read; a mistaken guess costs nothing more
// than a fresh session.
export async function createOrReuseSession(agentConfigId?: string): Promise<SessionRow> {
  try {
    const list = await api.sessions.list() as SessionRow[];
    const candidate = list.find(s => s.name === DEFAULT_SESSION_NAME);
    if (candidate) {
      const entries = await api.sessions.messages(candidate.id, { limit: 1 }) as unknown[];
      if (Array.isArray(entries) && entries.length === 0) return candidate;
    }
  } catch {
    // A failed look is not a failed create.
  }
  return await api.sessions.create(DEFAULT_SESSION_NAME, agentConfigId) as SessionRow;
}
