// activates reports whether a key press on a clickable row is the row's own
// Enter or Space. One from a control inside the row — a button, a menu item
// rendered through a portal — bubbles here too, and is that control's.
export function activates(e: { key: string; target: EventTarget | null; currentTarget: EventTarget | null }): boolean {
  return e.target === e.currentTarget && (e.key === 'Enter' || e.key === ' ');
}
