// Bus for injecting text into the active chat composer from elsewhere (quoting
// terminal output). Single-listener like toast: the mounted MessageInput
// registers itself, senders fire and forget; insert returns false when none is mounted.

import { loadDraft, saveDraft } from '@/lib/drafts';

type InsertListener = ((text: string) => void) | null;

let _listener: InsertListener = null;

export function onComposerInsert(fn: InsertListener): void { _listener = fn; }

export function insertIntoComposer(text: string): boolean {
  if (!_listener) return false;
  _listener(text);
  return true;
}

// putBackInComposer returns text a person typed to the box it came from: the
// open composer when it is that session's, else the session's saved draft.
export function putBackInComposer(sessionId: string, open: boolean, text: string): void {
  if (open && insertIntoComposer(text)) return;
  const draft = loadDraft(sessionId);
  saveDraft(sessionId, draft ? draft + '\n' + text : text);
}

// quoteAsCodeBlock wraps terminal output in a Markdown fence longer than any
// backtick run inside; trailing whitespace per line (xterm cell padding) is stripped.
export function quoteAsCodeBlock(raw: string): string {
  const text = raw
    .split('\n')
    .map(l => l.replace(/\s+$/, ''))
    .join('\n')
    .replace(/^\n+|\n+$/g, '');
  const longestRun = text.match(/`+/g)?.reduce((m, r) => Math.max(m, r.length), 0) ?? 0;
  const fence = '`'.repeat(Math.max(3, longestRun + 1));
  return fence + '\n' + text + '\n' + fence + '\n';
}
