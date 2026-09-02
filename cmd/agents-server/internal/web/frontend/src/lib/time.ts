// Local-time renderings of an RFC 3339 stamp; '' for none or unparseable.

function parse(iso: string | undefined): Date | null {
  if (!iso) return null;
  const d = new Date(iso);
  return isNaN(d.getTime()) ? null : d;
}

// formatTime is the one date-time format every list and label uses:
// "Sep 1, 07:19" (short) or "Sep 1, 07:19:42" (long), month and order in the
// viewer's locale, the year added only when it is not the current one.
export function formatTime(iso: string, style: 'short' | 'long' = 'short'): string {
  const d = parse(iso);
  if (!d) return '';
  const opts: Intl.DateTimeFormatOptions = {
    month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', hourCycle: 'h23',
  };
  if (style === 'long') opts.second = '2-digit';
  if (d.getFullYear() !== new Date().getFullYear()) opts.year = 'numeric';
  return d.toLocaleString(undefined, opts);
}

export function shortTime(iso?: string): string {
  return iso ? formatTime(iso) : '';
}

export function shortDate(iso?: string): string {
  const d = parse(iso);
  return d ? d.toLocaleDateString() : '';
}
