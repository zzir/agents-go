// Line diff for the Replay dialog's response comparison. LCS-based, no
// dependency: payloads are a few hundred lines, not megabytes.

export interface DiffLine {
  type: 'same' | 'add' | 'del';
  text: string;
}

// diffLines compares a (old/original) against b (new/replay) line by line.
// Deletions are lines only in a, additions lines only in b.
export function diffLines(a: string, b: string): DiffLine[] {
  const al = a === '' ? [] : a.split('\n');
  const bl = b === '' ? [] : b.split('\n');

  // Common prefix/suffix are peeled off first: they dominate real payload
  // pairs and keep the DP table small.
  let start = 0;
  while (start < al.length && start < bl.length && al[start] === bl[start]) start++;
  let endA = al.length;
  let endB = bl.length;
  while (endA > start && endB > start && al[endA - 1] === bl[endB - 1]) { endA--; endB--; }

  const midA = al.slice(start, endA);
  const midB = bl.slice(start, endB);
  const out: DiffLine[] = [];
  for (let i = 0; i < start; i++) out.push({ type: 'same', text: al[i] });

  // Guard the quadratic middle: past the cap, fall back to plain replace —
  // still correct, just not minimal.
  if (midA.length * midB.length > 1_000_000) {
    for (const t of midA) out.push({ type: 'del', text: t });
    for (const t of midB) out.push({ type: 'add', text: t });
  } else {
    // LCS lengths, then backtrack.
    const n = midA.length;
    const m = midB.length;
    const dp = new Uint32Array((n + 1) * (m + 1));
    const at = (i: number, j: number): number => i * (m + 1) + j;
    for (let i = n - 1; i >= 0; i--) {
      for (let j = m - 1; j >= 0; j--) {
        dp[at(i, j)] = midA[i] === midB[j]
          ? dp[at(i + 1, j + 1)] + 1
          : Math.max(dp[at(i + 1, j)], dp[at(i, j + 1)]);
      }
    }
    let i = 0;
    let j = 0;
    while (i < n && j < m) {
      if (midA[i] === midB[j]) { out.push({ type: 'same', text: midA[i] }); i++; j++; }
      else if (dp[at(i + 1, j)] >= dp[at(i, j + 1)]) { out.push({ type: 'del', text: midA[i] }); i++; }
      else { out.push({ type: 'add', text: midB[j] }); j++; }
    }
    while (i < n) { out.push({ type: 'del', text: midA[i++] }); }
    while (j < m) { out.push({ type: 'add', text: midB[j++] }); }
  }

  for (let i = endA; i < al.length; i++) out.push({ type: 'same', text: al[i] });
  return out;
}
