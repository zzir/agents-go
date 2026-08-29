export interface Skill {
  id: string;
  name: string;
  description: string;
  content?: string; // only on GET /skills/:id — the list carries metadata only
  source_repo?: string;
  source_path?: string;
  repo_label?: string;
  detached?: boolean;
  scope?: string;
  owner_id?: string;
}

export interface SkillGroup {
  repo: string; // import source URL; '' = authored in the workbench
  ownerId: string;
  label: string;
  // The group's visibility. An imported repo is one scope by invariant (the
  // whole group flips at once — spec §5.29); a Local group is a per-owner
  // bucket whose rows flip one at a time, so this is set only when uniform.
  scope?: string;
  skills: Skill[];
  key: string;
}

// The model-facing prefix of an imported skill: "owner/repo" for a github.com
// source, the host otherwise. Mirrors store.repoLabelOf — the server stores
// the authoritative value on the row (repo_label); this reproduces it for a
// row fetched before that field existed and for grouping.
export function repoLabel(repo: string): string {
  if (!repo) return '';
  let u: URL;
  try {
    u = new URL(repo);
  } catch {
    return repo;
  }
  if (u.host === 'github.com') {
    const parts = u.pathname.replace(/^\/|\/$/g, '').split('/');
    if (parts.length >= 2) return `${parts[0]}/${parts[1].replace(/\.git$/, '')}`;
  }
  return u.host;
}

// The name the model uses for a skill: qualified by its repo when imported.
// The server's stored label wins; repoLabel derives it otherwise.
export function qualifiedName(sk: Skill): string {
  const label = sk.repo_label || repoLabel(sk.source_repo || '');
  return label ? `${label}:${sk.name}` : sk.name;
}

// groupSkills buckets the listing the way scope moves: an imported repo is
// one group PER OWNER (the same repo imported by two people is two groups,
// each flipping on its own), and workbench-authored skills bucket by owner.
// Groups follow the scoped-listing order the flat panels use (store's
// scopedListOrder): published first, then whichever group was added most
// recently — the rows arrive in that order, so first-seen IS newest-first.
export function groupSkills(skills: Skill[]): SkillGroup[] {
  const map = new Map<string, SkillGroup>();
  for (const sk of skills) {
    const repo = sk.source_repo || '';
    const owner = sk.owner_id || '';
    const key = repo + '\u0000' + owner; // NUL: neither a URL nor a uuid holds one
    let group = map.get(key);
    if (!group) {
      group = {
        repo,
        ownerId: owner,
        label: repo ? (sk.repo_label || repoLabel(repo)) : 'Local',
        scope: sk.scope,
        skills: [],
        key,
      };
      map.set(key, group);
    }
    if (group.scope !== sk.scope) group.scope = undefined; // a Local bucket may be mixed
    group.skills.push(sk);
  }
  // Insertion order already carries the server's ordering; sorting again
  // would replace it with an alphabet nobody asked for. Only the published
  // groups are lifted, mirroring the flat listings' first cut.
  const groups = Array.from(map.values());
  return [...groups.filter(g => g.scope === 'global'), ...groups.filter(g => g.scope !== 'global')];
}
