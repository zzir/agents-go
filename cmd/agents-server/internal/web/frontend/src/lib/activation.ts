// activates reports whether a key press on a clickable row is the row's own
// Enter or Space; one from a control inside it (a portal menu item bubbles here
// too) is that control's.
export function activates(e: { key: string; target: EventTarget | null; currentTarget: EventTarget | null }): boolean {
  return e.target === e.currentTarget && (e.key === 'Enter' || e.key === ' ');
}
