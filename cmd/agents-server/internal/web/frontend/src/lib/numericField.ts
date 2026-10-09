// A numeric form field holds a string while editing and parses on save — invariant 80.

// numberDraft is the stored number as the field first shows it.
export function numberDraft(n: number | undefined | null): string {
  return n ? String(n) : '';
}

// parseWholeNumber reads a whole-number draft: empty is 0, and anything that
// is not an integer throws with the field's label, for the save to refuse.
export function parseWholeNumber(raw: string, label: string): number {
  const s = raw.trim();
  if (s === '') return 0;
  if (!/^-?\d+$/.test(s)) throw new Error(`${label} is not a whole number — fix or clear it before saving`);
  return Number(s);
}

// parseOptionalPositive reads a count that may be left out: empty is 0, the
// server's "none", and anything else must be a whole number of at least 1.
export function parseOptionalPositive(raw: string, label: string): number {
  const s = raw.trim();
  if (s === '') return 0;
  if (!/^\d+$/.test(s) || Number(s) < 1) throw new Error(`${label} must be a whole number of at least 1 — fix or clear it`);
  return Number(s);
}

// parseOptionalPositiveDecimal is parseOptionalPositive for a quantity that
// may carry a fraction (cpus): empty is 0, anything else a number above 0.
export function parseOptionalPositiveDecimal(raw: string, label: string): number {
  const s = raw.trim();
  if (s === '') return 0;
  if (!/^\d+(\.\d+)?$/.test(s) || Number(s) <= 0) throw new Error(`${label} must be a number above 0 — fix or clear it`);
  return Number(s);
}
