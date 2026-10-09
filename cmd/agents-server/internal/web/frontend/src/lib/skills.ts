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
  // The group's visibility: an imported repo is one scope (the whole group flips
  // at once — decisions §5.29); a Local bucket's rows flip one at a time, so this
  // is set only when uniform.
  scope?: string;
  skills: Skill[];
  key: string;
}

// The model-facing prefix of an imported skill: "owner/repo" for a github.com
// source, the host otherwise. Mirrors store.repoLabelOf; a row's repo_label is
// authoritative when present.
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

// groupSkills buckets the listing the way scope moves: an imported repo is one
// group PER OWNER, workbench-authored skills bucket by owner. Groups keep the
// scoped-listing order (store's scopedListOrder): published first, then newest.
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
  // Insertion order already carries the server's ordering; only the published
  // groups are lifted, as the flat listings do.
  const groups = Array.from(map.values());
  return [...groups.filter(g => g.scope === 'global'), ...groups.filter(g => g.scope !== 'global')];
}
