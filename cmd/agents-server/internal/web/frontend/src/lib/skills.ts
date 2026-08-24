export interface Skill {
  id: string;
  name: string;
  description: string;
  content?: string; // only on GET /skills/:id — the list carries metadata only
  source_repo?: string;
  source_path?: string;
  detached?: boolean;
  scope?: string;
  owner_id?: string;
}

export interface SkillGroup {
  repo: string; // import source URL; '' = authored in the workbench
  label: string;
  skills: Skill[];
  // Distinct render key when several groups share a repo (the admin view
  // splits the '' bucket per owner); defaults to repo.
  key?: string;
}

// Group skills by their import source; workbench-authored skills ("Local")
// sort first, then sources alphabetically.
export function groupBySource(skills: Skill[]): SkillGroup[] {
  const map = new Map<string, Skill[]>();
  for (const s of skills) {
    const repo = s.source_repo || '';
    if (!map.has(repo)) map.set(repo, []);
    map.get(repo)!.push(s);
  }
  return Array.from(map.entries())
    .map(([repo, skills]) => ({ repo, label: repo ? repo.replace(/^https?:\/\//, '') : 'Local', skills }))
    .sort((a, b) => (a.repo === '' ? -1 : b.repo === '' ? 1 : a.label.localeCompare(b.label)));
}

// splitLocalByOwner splits the admin view's "Local" bucket — otherwise a
// MIXED-ownership pile — so a group scope flip's blast radius is exactly what
// the heading says: "Local" keeps the admin's own and the global rows, and
// each other member's authored skills get their own labeled group.
export function splitLocalByOwner(
  groups: SkillGroup[],
  meId: string | undefined,
  labelFor: (ownerId: string) => string,
): SkillGroup[] {
  const local = groups.find(g => g.repo === '');
  if (!local) return groups;
  const mine: Skill[] = [];
  const byOwner = new Map<string, Skill[]>();
  for (const sk of local.skills) {
    if (sk.scope === 'global' || !sk.owner_id || sk.owner_id === meId) {
      mine.push(sk);
    } else {
      if (!byOwner.has(sk.owner_id)) byOwner.set(sk.owner_id, []);
      byOwner.get(sk.owner_id)!.push(sk);
    }
  }
  const split: SkillGroup[] = [];
  if (mine.length > 0) split.push({ ...local, skills: mine });
  for (const [owner, skills] of Array.from(byOwner.entries()).sort(([a], [b]) => labelFor(a).localeCompare(labelFor(b)))) {
    split.push({ repo: '', key: 'local-' + owner, label: `Local — ${labelFor(owner)}`, skills });
  }
  return [...split, ...groups.filter(g => g.repo !== '')];
}
