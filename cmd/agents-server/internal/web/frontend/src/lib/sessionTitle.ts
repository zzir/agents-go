// sessionTitle names a conversation a form makes for the work it starts: what
// the server would give a default-named one at its first workflow start
// ("<target>: <brief>", 40 characters at most), given now so two triggers'
// conversations can be told apart before either fires.
export function sessionTitle(target: string, brief: string): string {
  const b = brief.split(/\s+/).filter(Boolean).join(' ');
  const title = b ? `${target}: ${b}` : target;
  const chars = [...title];
  return chars.length > 40 ? chars.slice(0, 39).join('') + '…' : title;
}
