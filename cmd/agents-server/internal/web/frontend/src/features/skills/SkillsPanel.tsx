import { useState } from 'react';
import { Button, TextInput, Label, Stack, PageHeader, useConfirm } from '@primer/react';
import { Blankslate } from '@primer/react/experimental';
import { api } from '@/lib/api';
import { useApi } from '@/lib/hooks';
import { toast } from '@/lib/toast';
import { type Skill, groupByRepo } from '@/lib/skills';
import { BADGE } from '@/lib/badges';
import { Disclosure } from '@/components/Disclosure';

export function SkillsPanel() {
  const confirmDialog = useConfirm();
  const { data: skills, loading, error, reload } = useApi<Skill[]>(() => api.skills.list() as Promise<Skill[]>);
  const [repoUrl, setRepoUrl] = useState('');
  const [cloning, setCloning] = useState(false);
  const [adding, setAdding] = useState(false);
  const [updating, setUpdating] = useState('');
  // Repos are collapsed by default — a cloned repo can bundle dozens of
  // skills, so the list shows one row per directory (name + count) until
  // expanded. Same chevron mechanics as the agent form's skill picker.
  const [expandedRepos, setExpandedRepos] = useState<Set<string>>(new Set());
  const toggleRepo = (repo: string) => {
    setExpandedRepos(prev => {
      const next = new Set(prev);
      if (next.has(repo)) next.delete(repo); else next.add(repo);
      return next;
    });
  };

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
    const ok = await confirmDialog({
      title: `Delete “${topDir}”?`,
      content: 'The cloned repository and all its skills are removed. This cannot be undone.',
      confirmButtonContent: 'Delete',
      confirmButtonType: 'danger',
    });
    if (!ok) return;
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
              {/* A scan that failed says so — an empty list would read as
                  "nothing installed" while the workspace was merely unreadable. */}
              <Blankslate.Description>{error ? 'Could not scan skills: ' + error : 'No skills installed.'}</Blankslate.Description>
              {error && <Blankslate.PrimaryAction onClick={() => reload()}>Retry</Blankslate.PrimaryAction>}
            </Blankslate>
          </div>
        )}

        {grouped.map(group => (
          <div key={group.repo} className="Box">
            {/* The row is the toggle; Update/Delete ride inside it, so a div
                header (a <button> cannot nest them) with the clicks stopped. */}
            <Disclosure
              as="div"
              variant="plain"
              className="disclosure-row"
              open={expandedRepos.has(group.repo)}
              onToggle={() => toggleRepo(group.repo)}
              label={<>
                {group.repo}
                <Label variant={BADGE.count}>{'Skills·' + group.skills.length}</Label>
                <div className="resource-row-actions">
                  <Button
                    size="small"
                    variant="invisible"
                    onClick={e => { e.stopPropagation(); handleUpdate(group.repo); }}
                    disabled={updating === group.repo}
                  >
                    {updating === group.repo ? 'Updating...' : 'Update'}
                  </Button>
                  <Button size="small" variant="danger" onClick={e => { e.stopPropagation(); handleDelete(group.repo); }}>
                    Delete
                  </Button>
                </div>
              </>}
            >
              {group.skills.map(s => (
                <div key={s.path} className="Box-row">
                  <div className="resource-row-main">
                    <div className="resource-row-title">{s.name}</div>
                    {s.description && <div className="resource-row-sub">{s.description}</div>}
                  </div>
                </div>
              ))}
            </Disclosure>
          </div>
        ))}
      </>}
    </Stack>
  );
}

export default SkillsPanel;
