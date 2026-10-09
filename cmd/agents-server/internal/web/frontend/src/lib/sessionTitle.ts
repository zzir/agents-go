// sessionTitle names a session a form makes for the work it starts, as the server
// would at its first workflow start: "<target>: <brief>", clipped like
// `store.ClipName` — invariant 78.
export function sessionTitle(target: string, brief: string): string {
  const b = brief.split(/\s+/).filter(Boolean).join(' ');
  const title = b ? `${target}: ${b}` : target;
  const chars = [...title];
  return chars.length > 40 ? chars.slice(0, 39).join('') + '…' : title;
}
