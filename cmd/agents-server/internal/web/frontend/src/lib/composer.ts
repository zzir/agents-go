// Bus for injecting text into the active chat composer from elsewhere in the
// app (e.g. quoting terminal output into the input box). Same single-listener
// pattern as toast: the mounted MessageInput registers itself; senders fire
// and forget. insert returns false when no composer is mounted (no session
// open) so callers can surface a hint instead of silently dropping text.

type InsertListener = ((text: string) => void) | null;

let _listener: InsertListener = null;

export function onComposerInsert(fn: InsertListener): void { _listener = fn; }

export function insertIntoComposer(text: string): boolean {
  if (!_listener) return false;
  _listener(text);
  return true;
}

// quoteAsCodeBlock wraps raw terminal output in a Markdown code fence, using
// a fence longer than any backtick run inside so the content can't break out.
// Trailing whitespace per line (xterm selections keep cell padding) is
// stripped.
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
