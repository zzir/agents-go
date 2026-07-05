export interface Skill {
  name: string;
  description: string;
  path: string;
}

export interface SkillGroup {
  repo: string;
  skills: Skill[];
}

// A skill's path is "<repo>/<name>/SKILL.md" for a cloned repo (or just
// "<name>/SKILL.md" for a loose, non-cloned skill), so the first path segment
// is the directory to group and bulk-select by.
export function groupByRepo(skills: Skill[]): SkillGroup[] {
  const map = new Map<string, Skill[]>();
  for (const s of skills) {
    const repo = s.path.includes('/') ? s.path.split('/')[0] : s.path;
    if (!map.has(repo)) map.set(repo, []);
    map.get(repo)!.push(s);
  }
  return Array.from(map.entries()).map(([repo, skills]) => ({ repo, skills }));
}
