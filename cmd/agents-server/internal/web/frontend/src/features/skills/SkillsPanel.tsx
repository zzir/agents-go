import { useState, useEffect, useCallback } from 'react';
import { Button, TextInput, Label, Stack, PageHeader } from '@primer/react';
import { Blankslate } from '@primer/react/experimental';
import { api } from '@/lib/api';
import { toast } from '@/lib/toast';
import { type Skill, groupByRepo } from '@/lib/skills';

export function SkillsPanel() {
  const [skills, setSkills] = useState<Skill[] | null>(null);
  const [loading, setLoading] = useState(true);
  const [repoUrl, setRepoUrl] = useState('');
  const [cloning, setCloning] = useState(false);
  const [adding, setAdding] = useState(false);
  const [updating, setUpdating] = useState('');
  const reload = useCallback(() => {
    setLoading(true);
    api.skills.list()
      .then((data: unknown) => setSkills((data as Skill[]) || []))
      .catch(() => setSkills([]))
      .finally(() => setLoading(false));
  }, []);

  useEffect(() => { reload(); }, [reload]);

  const handleClone = async () => {
    const url = repoUrl.trim();
    if (!url) return;
    setCloning(true);
    try {
      await api.skills.clone(url);
      setRepoUrl('');
      setAdding(false);
      reload();
    } catch (e) {
      toast.error((e as Error).message || 'Clone failed');
    } finally {
      setCloning(false);
    }
  };

  const handleUpdate = async (topDir: string) => {
    setUpdating(topDir);
    try {
      await api.skills.update(topDir);
      reload();
    } catch (e) {
      toast.error((e as Error).message || 'Update failed');
    } finally {
      setUpdating('');
    }
  };

  const handleDelete = async (topDir: string) => {
    if (!confirm('Delete "' + topDir + '"?')) return;
    try {
      await api.skills.delete(topDir);
      reload();
    } catch (e) {
      toast.error((e as Error).message || 'Delete failed');
    }
  };

  const grouped = groupByRepo(skills || []);

  return (
    <Stack gap="normal">
      <PageHeader>
        <PageHeader.TitleArea>
          <PageHeader.Title>Skills</PageHeader.Title>
        </PageHeader.TitleArea>
        {!adding && <PageHeader.Actions><Button onClick={() => setAdding(true)} variant="primary" size="small">+ Add</Button></PageHeader.Actions>}
      </PageHeader>

      {adding && (
        <Stack gap="normal" direction="horizontal" align="center">
          <TextInput block
            placeholder="Git repository URL"
            value={repoUrl}
            onChange={e => setRepoUrl(e.target.value)}
            onKeyDown={e => { if (e.key === 'Enter' && !cloning) handleClone(); }}
            disabled={cloning}
            style={{ flex: 1 }}
          />
          <Button variant="primary" size="small" onClick={handleClone} disabled={cloning || !repoUrl.trim()}>
            {cloning ? 'Cloning...' : 'Clone'}
          </Button>
          <Button size="small" onClick={() => { setAdding(false); setRepoUrl(''); }}>Cancel</Button>
        </Stack>
      )}

      {!adding && <>
        {loading && (
          <div className="Box">
            <Blankslate>
              <Blankslate.Description>Scanning...</Blankslate.Description>
            </Blankslate>
          </div>
        )}

        {!loading && grouped.length === 0 && (
          <div className="Box">
            <Blankslate>
              <Blankslate.Description>No skills installed.</Blankslate.Description>
            </Blankslate>
          </div>
        )}

        {grouped.map(group => (
          <div key={group.repo} className="Box">
            <div className="Box-row" style={{ fontWeight: 500 }}>
              <span>{group.repo}</span>
              <div className="resource-row-actions">
                <Button
                  size="small"
                  variant="invisible"
                  onClick={() => handleUpdate(group.repo)}
                  disabled={updating === group.repo}
                >
                  {updating === group.repo ? 'Updating...' : 'Update'}
                </Button>
                <Button size="small" variant="danger" onClick={() => handleDelete(group.repo)}>
                  Delete
                </Button>
              </div>
            </div>
            {group.skills.map(s => (
              <div key={s.path} className="Box-row">
                <div className="resource-row-main">
                  <div className="resource-row-title">{s.name}</div>
                  {s.description && <div className="resource-row-sub">{s.description}</div>}
                </div>
              </div>
            ))}
          </div>
        ))}
      </>}
    </Stack>
  );
}

export default SkillsPanel;
