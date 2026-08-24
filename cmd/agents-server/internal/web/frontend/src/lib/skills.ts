export interface Skill {
  id: string;
  name: string;
  description: string;
  content?: string; // only on GET /skills/:id — the list carries metadata only
  source_repo?: string;
  source_path?: string;
  detached?: boolean;
}

export interface SkillGroup {
  repo: string; // import source URL; '' = authored in the workbench
  label: string;
  skills: Skill[];
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
